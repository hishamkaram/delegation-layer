package infrastructurehost

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// The command invocation owns this immutable result. Dependency copies and
// selected launch scopes share it; a capture fault matters only to work which
// needs infrastructure routing or outside establishment.
type Invocation struct {
	source infrastructure.InvocationSource
	err    error
}

func CaptureInvocation(ctx context.Context) *Invocation {
	// This context covers only the immediate source observation, never target
	// work or a resident lifetime. Entries without a context use Background.
	source, err := infrastructure.CaptureInvocationSource(ctx)
	return &Invocation{source: source, err: err}
}
