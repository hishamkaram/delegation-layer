package control

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// unrecordedDeadlineContext has a passed deadline that its Err does not yet
// report. The dialer detects a passed deadline from the clock on its own, so
// this is the caller state a probe can observe at its deadline.
type unrecordedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (ctx unrecordedDeadlineContext) Deadline() (time.Time, bool) {
	return ctx.deadline, true
}

// TestPrivateEndpointProbeKeepsCallerTermination proves that a probe ended by
// the caller's own cancellation or deadline reports that termination. It is
// neither an absence, a refusal, nor a binding fault, for a refused socket
// and for a missing path alike.
func TestPrivateEndpointProbeKeepsCallerTermination(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	paths := map[string]string{
		"refused": fixture.socketPath,
		"missing": filepath.Join(filepath.Dir(fixture.socketPath), "missing"),
	}
	terminations := map[string]struct {
		context func() context.Context
		cause   error
	}{
		"cancelled": {
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			cause: context.Canceled,
		},
		"expired": {
			context: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
				t.Cleanup(cancel)
				return ctx
			},
			cause: context.DeadlineExceeded,
		},
		"deadline passed before its context records it": {
			context: func() context.Context {
				return unrecordedDeadlineContext{Context: context.Background(), deadline: time.Unix(0, 0)}
			},
			cause: context.DeadlineExceeded,
		},
	}
	for pathName, path := range paths {
		for terminationName, termination := range terminations {
			t.Run(pathName+" "+terminationName, func(t *testing.T) {
				unavailable, refused, err := privateEndpointProbe(termination.context(), path)
				if unavailable || refused || !errors.Is(err, termination.cause) || errors.Is(err, ErrBinding) {
					t.Fatalf("probe = unavailable %t refused %t err %v, want only %v", unavailable, refused, err, termination.cause)
				}
				var dialErr *net.OpError
				if !errors.As(err, &dialErr) || dialErr.Op != "dial" || dialErr.Net != "unix" {
					t.Fatalf("probe error = %v, want the original unix dial error", err)
				}
			})
		}
	}
	fixture.assertSocketPreserved(t)
}
