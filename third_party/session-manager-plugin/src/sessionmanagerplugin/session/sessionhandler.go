// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package session starts the session.
package session

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/session-manager-plugin/src/communicator"
	"github.com/aws/session-manager-plugin/src/config"
	"github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/session-manager-plugin/src/message"
	"github.com/aws/session-manager-plugin/src/retry"
	"github.com/aws/session-manager-plugin/src/sdkutil"
)

// OpenDataChannel initializes datachannel
func (s *Session) OpenDataChannel(log log.T) (err error) {
	s.retryParams = retry.RepeatableExponentialRetryer{
		GeometricRatio:      config.RetryBase,
		InitialDelayInMilli: rand.Intn(config.DataChannelRetryInitialDelayMillis) + config.DataChannelRetryInitialDelayMillis,
		MaxDelayInMilli:     config.DataChannelRetryMaxIntervalMillis,
		MaxAttempts:         config.DataChannelNumMaxRetries,
	}

	// Check environment variable to control client configuration
	// SSM_PLUGIN_SKIP_CLIENT_CONFIGURE:
	//   - not set or "false": perform client configuration (default behavior)
	//   - "true": skip client configuration
	skipClientConfigure := os.Getenv("SSM_PLUGIN_SKIP_CLIENT_CONFIGURE")
	if skipClientConfigure != "true" {
		// Check if the StreamUrl is presigned first to avoid unnecessary credential lookup delay
		presigned, err := communicator.IsPresignedURL(s.StreamUrl)
		if err != nil {
			log.Errorf("Failed to check if URL is presigned: %v", err)
		}

		// Only attempt credential lookup if URL is not presigned
		if !presigned {
			ctx := context.Background()
			cfg, err := sdkutil.GetConfigWithQuickCheck(ctx)
			if err != nil {
				log.Errorf("Failed to create aws config: %v", err)
			} else {
				creds, err := cfg.Credentials.Retrieve(ctx)
				if err != nil {
					log.Errorf("Failed to get credential for sign: %v", err)
				} else {
					s.Signer = v4.NewSigner()
					s.Credentials = creds
				}
			}
		} else {
			log.Debugf("StreamUrl is presigned, skipping credential lookup")
		}
	} else {
		log.Debugf("Client configuration skipped by SSM_PLUGIN_SKIP_CLIENT_CONFIGURE=%s", skipClientConfigure)
	}

	s.DataChannel.Initialize(log, s.ClientId, s.SessionId, s.TargetId, s.IsAwsCliUpgradeNeeded)
	s.DataChannel.SetWebsocket(log, s.StreamUrl, s.TokenValue, s.Region, s.Signer, s.Credentials)
	s.DataChannel.GetWsChannel().SetOnMessage(
		func(input []byte) {
			s.DataChannel.OutputMessageHandler(log, s.Stop, s.SessionId, input)
		})
	s.DataChannel.RegisterOutputStreamHandler(s.ProcessFirstMessage, false)

	if err = s.DataChannel.Open(log); err != nil {
		log.Errorf("Retrying connection for data channel id: %s failed with error: %s", s.SessionId, err)
		s.retryParams.CallableFunc = func() (err error) { return s.DataChannel.Reconnect(log) }
		if err = s.retryParams.Call(); err != nil {
			log.Error(err)
		}
	}

	s.DataChannel.GetWsChannel().SetOnError(
		func(err error) {
			log.Errorf("Trying to reconnect the session: %v with seq num: %d", s.StreamUrl, s.DataChannel.GetStreamDataSequenceNumber())
			s.retryParams.CallableFunc = func() (err error) { return s.ResumeSessionHandler(log) }
			if err = s.retryParams.Call(); err != nil {
				log.Error(err)
				// NAKAMA PATCH: reconnect retries are exhausted; upstream only
				// logs and leaves the session hanging. Report a permanent
				// disconnect when SSM_PLUGIN_DISCONNECT_EXIT_CODE is set
				// (no-op otherwise).
				nakamaExitOnPermanentDisconnect()
			}
		})

	// Scheduler for resending of data
	s.DataChannel.ResendStreamDataMessageScheduler(log)

	return nil
}

// ProcessFirstMessage only processes messages with PayloadType Output to determine the
// sessionType of the session to be launched. This is a fallback for agent versions that do not support handshake, they
// immediately start sending shell output.
func (s *Session) ProcessFirstMessage(log log.T, outputMessage message.ClientMessage) (isHandlerReady bool, err error) {
	// Immediately deregister self so that this handler is only called once, for the first message
	s.DataChannel.DeregisterOutputStreamHandler(s.ProcessFirstMessage)
	// Only set session type if the session type has not already been set. Usually session type will be set
	// by handshake protocol which would be the first message but older agents may not perform handshake
	if s.SessionType == "" {
		if outputMessage.PayloadType == uint32(message.Output) {
			log.Warn("Setting session type to shell based on PayloadType!")
			s.DataChannel.SetSessionType(config.ShellPluginName)
			s.DisplayMode.DisplayMessage(log, outputMessage)
		}
	}
	return true, nil
}

// NAKAMA PATCH: nakamaExitOnPermanentDisconnect terminates the process
// with the exit code given in the SSM_PLUGIN_DISCONNECT_EXIT_CODE
// environment variable. nakama's `aws connect` supervisor sets it on the
// child session process so that permanent disconnects (session timed out
// on the server, reconnect retries exhausted, stream resend timeout) are
// distinguishable from a normal session end (exit 0). When the variable
// is unset or not an integer this function is a no-op, preserving the
// upstream behavior exactly (same gating style as
// SSM_PLUGIN_SKIP_CLIENT_CONFIGURE in OpenDataChannel above).
func nakamaExitOnPermanentDisconnect() {
	if v := os.Getenv("SSM_PLUGIN_DISCONNECT_EXIT_CODE"); v != "" {
		if code, err := strconv.Atoi(v); err == nil {
			os.Exit(code)
		}
	}
}

// Stop will end the session
func (s *Session) Stop() {
	os.Exit(0)
}

// GetResumeSessionParams calls ResumeSession API and gets tokenvalue for reconnecting
func (s *Session) GetResumeSessionParams(log log.T) (string, string, error) {
	ctx := context.Background()

	var (
		resumeSessionOutput *ssm.ResumeSessionOutput
		err                 error
	)

	// Check if the StreamUrl is presigned first to avoid unnecessary credential lookup delay
	presigned, err := communicator.IsPresignedURL(s.StreamUrl)
	if err != nil {
		log.Errorf("Failed to check if URL is presigned: %v", err)
		presigned = false
	}

	// Only attempt credential lookup if URL is not presigned
	if !presigned {
		cfg, err := sdkutil.GetConfigWithQuickCheck(ctx)
		if err != nil {
			return "", "", err
		}

		var ssmOpts []func(*ssm.Options)
		if s.Endpoint != "" {
			endpoint := s.Endpoint
			ssmOpts = append(ssmOpts, func(o *ssm.Options) {
				o.BaseEndpoint = &endpoint
			})
		}
		ssmClient := ssm.NewFromConfig(cfg, ssmOpts...)

		// Update signer with fresh credentials
		creds, getCredErr := cfg.Credentials.Retrieve(ctx)
		if getCredErr != nil {
			log.Errorf("Failed to get credential for sign")
		} else {
			s.Signer = v4.NewSigner()
			s.Credentials = creds
		}

		resumeSessionInput := ssm.ResumeSessionInput{
			SessionId: &s.SessionId,
		}

		log.Debugf("Resume Session input parameters: %v", resumeSessionInput)
		if resumeSessionOutput, err = ssmClient.ResumeSession(ctx, &resumeSessionInput); err != nil {
			log.Errorf("Resume Session failed: %v", err)
			return "", "", err
		}

		if resumeSessionOutput.TokenValue == nil {
			return "", "", nil
		}

		streamUrl := ""
		if resumeSessionOutput.StreamUrl != nil {
			streamUrl = *resumeSessionOutput.StreamUrl
		}

		return *resumeSessionOutput.TokenValue, streamUrl, nil
	}

	log.Debugf("StreamUrl is presigned, skipping resume session")
	return "", "", errors.New("Skip resuming session with presigned URL")
}

// getResumeSessionParams is the function used by ResumeSessionHandler to get token and stream URL.
// It is a variable to allow test injection.
var getResumeSessionParams = func(s *Session, log log.T) (string, string, error) {
	return s.GetResumeSessionParams(log)
}

// ResumeSessionHandler gets token value and tries to Reconnect to datachannel
func (s *Session) ResumeSessionHandler(log log.T) (err error) {
	var streamUrl string
	s.TokenValue, streamUrl, err = getResumeSessionParams(s, log)
	if err != nil {
		log.Errorf("Failed to get token: %v", err)
		return
	} else if s.TokenValue == "" {
		log.Debugf("Session: %s timed out", s.SessionId)
		fmt.Fprintf(os.Stdout, "Session: %s timed out.\n", s.SessionId)
		// NAKAMA PATCH: the server-side session is gone (typical after EC2
		// hibernate/stop); report it as a permanent disconnect when
		// SSM_PLUGIN_DISCONNECT_EXIT_CODE is set, else fall through to the
		// upstream os.Exit(0).
		nakamaExitOnPermanentDisconnect()
		os.Exit(0)
	}
	s.DataChannel.GetWsChannel().SetChannelToken(s.TokenValue)
	s.DataChannel.GetWsChannel().SetCredentials(s.Credentials)
	if streamUrl != "" {
		s.StreamUrl = streamUrl
		s.DataChannel.GetWsChannel().SetStreamUrl(streamUrl)
	}
	err = s.DataChannel.Reconnect(log)
	return
}

// TerminateSession calls TerminateSession API
func (s *Session) TerminateSession(log log.T) error {
	ctx := context.Background()

	cfg, err := sdkutil.GetConfigWithQuickCheck(ctx)
	if err != nil {
		log.Errorf("Terminate Session failed: %v", err)
		return err
	}

	var ssmOpts []func(*ssm.Options)
	if s.Endpoint != "" {
		endpoint := s.Endpoint
		ssmOpts = append(ssmOpts, func(o *ssm.Options) {
			o.BaseEndpoint = &endpoint
		})
	}
	ssmClient := ssm.NewFromConfig(cfg, ssmOpts...)

	terminateSessionInput := ssm.TerminateSessionInput{
		SessionId: &s.SessionId,
	}

	log.Debugf("Terminate Session input parameters: %v", terminateSessionInput)
	if _, err = ssmClient.TerminateSession(ctx, &terminateSessionInput); err != nil {
		log.Errorf("Terminate Session failed: %v", err)
		return err
	}
	return nil
}
