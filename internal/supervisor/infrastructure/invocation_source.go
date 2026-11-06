package infrastructure

import (
	"context"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// InvocationSource owns an immutable observation of the actual command entry.
// Capture it before opening the target or applying selected launch environments.
// Its zero value is invalid. An origin locator supplies routing data only;
// outside capture requires a separate native containment check.
type InvocationSource struct {
	origin  Origin
	outside capturedProducerStarter
}

func CaptureInvocationSource(ctx context.Context) (InvocationSource, error) {
	if ctx == nil {
		return InvocationSource{}, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return InvocationSource{}, err
	}
	environment := os.Environ()
	origin, found, err := ReadOrigin(environment)
	if err != nil {
		// Codec errors may quote environment content; expose only the safe category.
		return InvocationSource{}, fmt.Errorf("%w: invalid invocation origin", ErrIdentity)
	}
	if found {
		return InvocationSource{origin: origin}, nil
	}
	outside, err := captureOutsideProducer(ctx, environment)
	if err != nil {
		return InvocationSource{}, err
	}
	return InvocationSource{outside: outside}, nil
}

// Origin returns the original invocation's locator, never native authority.
func (s InvocationSource) Origin() (Origin, bool) {
	return s.origin, s.origin.root != ""
}

// EstablishOutside uses only this invocation's captured outside environment.
// Any nonnil attempt transfers cleanup ownership, including on error; the
// caller retains the borrowed target Store until the attempt is closed.
func (s InvocationSource) EstablishOutside(ctx context.Context, target *taskdir.Store, request EstablishmentRequest, host EstablishmentHost) (*StartupAttempt, error) {
	return s.establishOutside(ctx, target, request, host, producerNative{})
}

func (s InvocationSource) establishOutside(ctx context.Context, target *taskdir.Store, request EstablishmentRequest, host EstablishmentHost, native producerNative) (*StartupAttempt, error) {
	if ctx == nil || target == nil || host == nil {
		return nil, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.origin.root != "" {
		return nil, fmt.Errorf("%w: invocation origin requires owner routing", ErrIdentity)
	}
	if s.outside.environment == nil {
		return nil, os.ErrInvalid
	}
	return establishCaptured(ctx, target, request, capturedEstablishment{starter: s.outside, prepare: host.PrepareOutsideRunner}, native)
}
