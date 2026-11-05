package infrastructure

import (
	"context"
	"os"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Captured before target work, with no source-root resource guard retained.
// Only trusted outside entry or an authenticated resident can construct it.
type capturedProducerStarter struct {
	provenance  StartupProvenance
	environment []string
}

type capturedEstablishment struct {
	starter capturedProducerStarter
	prepare func(context.Context, *taskdir.Store) (Artifacts, error)
}

// Validate against the same pin establishment retains, before opening its
// journal or preparing artifacts. Provenance owns kind/root validation; this
// projection supplies only the target fields that its contract consumes.
func (s capturedProducerStarter) validateTarget(pin taskdir.RootPin) error {
	provenance := s.provenance
	if provenance.Kind == StarterOutside {
		if provenance.Root != "" || provenance.RootID != "" {
			return ErrIdentity
		}
		provenance.Root, provenance.RootID = pin.Root(), pin.RootID()
	}
	return provenance.validate(StartupInputs{Kind: StartupRootOwner, Root: pin.Root(), RootID: pin.RootID()})
}

func captureOutsideProducer(ctx context.Context, environment []string) (capturedProducerStarter, error) {
	if err := lifetime.CheckCurrentTaskContainment(ctx, environment); err != nil {
		return capturedProducerStarter{}, err
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return capturedProducerStarter{}, err
	}
	return capturedProducerStarter{
		provenance:  StartupProvenance{Kind: StarterOutside, PID: self.PID(), StartToken: self.StartToken()},
		environment: slices.Clone(environment),
	}, nil
}

// This is not a public provenance constructor. Authentication is rechecked on
// the actual resident, and every source guard is dropped before B is opened.
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
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return capturedProducerStarter{}, err
	}
	intent := e.operation.Intent()
	return capturedProducerStarter{
		provenance: StartupProvenance{
			Kind: StarterOwner, Root: intent.Inputs.Root, RootID: intent.Inputs.RootID,
			Epoch: intent.Epoch, PID: self.PID(), StartToken: self.StartToken(),
		},
		environment: slices.Clone(environment),
	}, nil
}
