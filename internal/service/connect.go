package service

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/twinj/uuid"

	"github.com/aws/session-manager-plugin/src/datachannel"
	smplog "github.com/aws/session-manager-plugin/src/log"
	smpsession "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"

	// Blank imports register the Standard_Stream (shell) and Port
	// session handlers into the plugin's SessionRegistry via their
	// package init() functions.
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/portsession"
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/shellsession"
)

// ConnectInput selects the connect mode. The three modes are mutually
// exclusive: a non-empty Command runs a command; a non-empty LocalPort
// starts a port-forward; otherwise an interactive shell is opened.
type ConnectInput struct {
	Name       string
	Command    []string // empty => interactive shell
	TTY        bool     // run Command in an interactive PTY
	LocalPort  string   // port-forward: local listener port
	RemotePort string   // port-forward: target port
	RemoteHost string   // optional: forward through the instance to this host
}

// Connect opens an AWS Systems Manager Session Manager session to the
// nakama instance and drives it with the vendored session-manager-plugin
// (see third_party/session-manager-plugin). Credentials resolved by
// nakama (including the `aws login` fallback) are injected into the
// session so the plugin does not run its own credential chain.
func (s *Service) Connect(ctx context.Context, in ConnectInput) error {
	if err := ValidateInstanceName(in.Name); err != nil {
		return err
	}
	instanceID := s.ssmGetOptional(ctx, ssmInstanceIDPath(in.Name))
	if instanceID == "" {
		return fmt.Errorf("nakama instance %q not found", in.Name)
	}

	docName, params := connectDocument(in)
	out, err := s.clients.SSM.StartSession(ctx, &ssm.StartSessionInput{
		Target:       aws.String(instanceID),
		DocumentName: docName,
		Parameters:   params,
	})
	if err != nil {
		return fmt.Errorf("StartSession: %w", err)
	}

	creds, err := s.clients.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("resolve credentials: %w", err)
	}
	// Tell the vendored plugin to skip its own credential lookup in
	// OpenDataChannel and use the Signer/Credentials we set below.
	// Scope the variable to this call so it is not inherited by any
	// later child process (CWE-526).
	defer func() { _ = os.Unsetenv("SSM_PLUGIN_SKIP_CLIENT_CONFIGURE") }()
	_ = os.Setenv("SSM_PLUGIN_SKIP_CLIENT_CONFIGURE", "true")

	uuid.SwitchFormat(uuid.CleanHyphen)
	logger := smplog.Logger(false, "nakama-connect")
	sess := &smpsession.Session{
		SessionId:   aws.ToString(out.SessionId),
		StreamUrl:   aws.ToString(out.StreamUrl),
		TokenValue:  aws.ToString(out.TokenValue),
		Endpoint:    fmt.Sprintf("https://ssm.%s.amazonaws.com", s.clients.Region),
		ClientId:    uuid.NewV4().String(),
		TargetId:    instanceID,
		Region:      s.clients.Region,
		DataChannel: &datachannel.DataChannel{},
		Signer:      v4.NewSigner(),
		Credentials: creds,
	}
	return sess.Execute(logger)
}

// connectUser is the OS user that connect sessions land in. Session
// Manager authenticates as ssm-user, which has passwordless sudo on
// managed nodes by default (the SSM agent writes
// "ssm-user ALL=(ALL) NOPASSWD:ALL" to /etc/sudoers.d/ssm-agent-users),
// so nakama switches to the Ubuntu default user that code-server runs
// as. See ADR 0009.
const connectUser = "ubuntu"

// connectDocument maps a ConnectInput to the SSM document name and
// parameters.
//
// Shell and command modes run as the ubuntu user rather than the
// ssm-user that Session Manager logs in as:
//   - The interactive shell uses AWS-StartInteractiveCommand running
//     "exec sudo su - ubuntu". exec replaces the ssm-user shell, so a
//     single "exit" ends the session (there is no parent shell to fall
//     back to).
//   - Commands run via "sudo su - ubuntu -c '<command>'"; the command
//     is single-quoted so the outer shell does not re-split or expand
//     it.
//
// Port-forwarding carries no user context and is unchanged.
func connectDocument(in ConnectInput) (*string, map[string][]string) {
	switch {
	case in.LocalPort != "":
		params := map[string][]string{
			"portNumber":      {in.RemotePort},
			"localPortNumber": {in.LocalPort},
		}
		if in.RemoteHost != "" {
			params["host"] = []string{in.RemoteHost}
			return aws.String("AWS-StartPortForwardingSessionToRemoteHost"), params
		}
		return aws.String("AWS-StartPortForwardingSession"), params
	case len(in.Command) > 0:
		doc := "AWS-StartNonInteractiveCommand"
		if in.TTY {
			doc = "AWS-StartInteractiveCommand"
		}
		command := fmt.Sprintf("sudo su - %s -c %s", connectUser, shellQuote(strings.Join(in.Command, " ")))
		return aws.String(doc), map[string][]string{"command": {command}}
	default:
		command := fmt.Sprintf("exec sudo su - %s", connectUser)
		return aws.String("AWS-StartInteractiveCommand"), map[string][]string{"command": {command}}
	}
}

// shellQuote returns s as a single POSIX shell word: wrapped in single
// quotes with embedded single quotes escaped as '\”. This makes s safe
// to splice into a command line without word-splitting or expansion.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
