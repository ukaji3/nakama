package awshelper

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrPollTimeout is returned by Poll when the deadline elapses
// without the predicate ever returning done=true.
var ErrPollTimeout = errors.New("polling timed out")

// PollOptions controls the behavior of Poll.
type PollOptions struct {
	// Interval is the time between successive predicate evaluations.
	Interval time.Duration
	// Timeout is the maximum total wall-clock time Poll will spend.
	Timeout time.Duration
	// Description is included in error messages.
	Description string
}

// Poll repeatedly calls fn until it reports done=true, returns an
// error, or the timeout elapses.
//
// fn signature:
//
//	(done bool, err error)
//	  done=true, err=nil  -> success, Poll returns nil immediately
//	  done=false, err=nil -> keep polling
//	  err != nil          -> abort, Poll returns the error
//
// Poll respects ctx.Done(): if the context is cancelled before
// completion, ctx.Err() is returned.
func Poll(ctx context.Context, opts PollOptions, fn func(context.Context) (bool, error)) error {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}

	pollCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// First attempt fires immediately so that already-ready
	// resources don't pay the interval delay.
	for {
		done, err := fn(pollCtx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				if opts.Description != "" {
					return fmt.Errorf("%s: %w", opts.Description, ErrPollTimeout)
				}
				return ErrPollTimeout
			}
			return pollCtx.Err()
		case <-time.After(opts.Interval):
		}
	}
}
