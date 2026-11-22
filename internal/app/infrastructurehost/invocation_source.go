package infrastructurehost

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// The command invocation owns this immutable result. Dependency copies and
// selected launch scopes share it; a capture fault matters only to work which
// needs infrastructure routing or outside establishment.
type Invocation struct {
	source infrastructure.InvocationSource
	err    error
}

// ValidateUpgrade checks the original capture before maintenance replay or work.
// It never recaptures a caller or removes its contained invocation origin.
func (i *Invocation) ValidateUpgrade(ctx context.Context) error {
	if i == nil || ctx == nil {
		return os.ErrInvalid
	}
	if _, finite := ctx.Deadline(); !finite {
		return os.ErrInvalid
	}
	if i.err != nil {
		return i.err
	}
	if _, nested := i.source.Origin(); nested {
		return infrastructure.ErrOwnerRequest
	}
	return ctx.Err()
}

func CaptureInvocation(ctx context.Context) *Invocation {
	// This context covers only the immediate source observation, never target
	// work or a resident lifetime. Entries without a context use Background.
	source, err := infrastructure.CaptureInvocationSource(ctx)
	return &Invocation{source: source, err: err}
}
