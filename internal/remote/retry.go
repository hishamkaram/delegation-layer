package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

const MaxAttempts = 5

// RetryPolicy owns only connection retry timing. It never retries execution.
// Sleep is injected for deterministic tests; nil selects a context-aware timer.
type RetryPolicy struct {
	Sleep func(context.Context, time.Duration) error
}

func (p RetryPolicy) Wait(ctx context.Context, failedAttempt int) error {
	if failedAttempt < 1 || failedAttempt >= MaxAttempts {
		return fmt.Errorf("%w: invalid retry attempt", ErrProtocol)
	}
	delay := time.Second << (failedAttempt - 1)
	if p.Sleep != nil {
		return p.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Transient recognizes connection loss only. Protocol, trust and authentication
// failures deliberately have no retry classification.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// BeforeSendError is returned only when the exchange proves no request bytes
// were sent (for example dialing failed). It permits retrying the same admit
// within this invocation; it never changes durable restart reconciliation.
type BeforeSendError struct{ Err error }

func (e *BeforeSendError) Error() string { return "remote request not sent: " + e.Err.Error() }
func (e *BeforeSendError) Unwrap() error { return e.Err }
func beforeSend(err error) bool {
	var failure *BeforeSendError
	return errors.As(err, &failure)
}

// VerifiedResponseError reports ancillary cleanup after the entire response was
// received over the authenticated channel. The coordinator still validates it.
type VerifiedResponseError struct{ Err error }

func (e *VerifiedResponseError) Error() string {
	return "remote response received; connection cleanup incomplete"
}
func (e *VerifiedResponseError) Unwrap() error { return e.Err }
