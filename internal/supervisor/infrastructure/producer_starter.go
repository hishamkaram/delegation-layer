package infrastructure

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Captured before target work, with no source-root resource guard retained.
// Only trusted outside entry or an authenticated resident can construct it.
// A nonnil environment distinguishes successful capture, including empty input.
type capturedProducerStarter struct {
	environment []string
}

type capturedEstablishment struct {
	starter capturedProducerStarter
	prepare func(context.Context, *taskdir.Store) (string, error)
}

func captureOutsideProducer(ctx context.Context, environment []string) (capturedProducerStarter, error) {
	if err := lifetime.CheckCurrentTaskContainment(ctx, environment); err != nil {
		return capturedProducerStarter{}, err
	}
	return capturedProducerStarter{
		environment: append([]string{}, environment...),
	}, nil
}

// Authentication is rechecked on the actual resident, and every source guard
// is dropped before B is opened.
// Accepted server work must stay registered until its target work has finished.
func (e *ownedResidentEntry) captureOwnerProducer(ctx context.Context) (capturedProducerStarter, error) {
	if e == nil || ctx == nil {
		return capturedProducerStarter{}, os.ErrInvalid
	}
	environment := os.Environ()
	if err := lifetime.CheckCurrentTaskContainment(ctx, environment); err != nil {
		return capturedProducerStarter{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.grant == nil || e.grant.entry != e || e.journal == nil || e.lease == nil {
		return capturedProducerStarter{}, os.ErrClosed
	}
	if err := e.validateResident(ctx); err != nil {
		return capturedProducerStarter{}, err
	}
	return capturedProducerStarter{
		environment: append([]string{}, environment...),
	}, nil
}
