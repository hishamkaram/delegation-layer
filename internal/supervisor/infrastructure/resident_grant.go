package infrastructure

import (
	"context"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

// residentGrant borrows the fixed entry's resources and closure guard. Neither
// DTOs, arbitrary inherited descriptors nor exported constructors can mint it.
type residentGrant struct{ entry *ownedResidentEntry }

func (g *residentGrant) GuardDaemonStart(ctx context.Context, operationRoot string, start func() error) error {
	if g == nil || g.entry == nil || ctx == nil || start == nil {
		return os.ErrInvalid
	}
	e := g.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return os.ErrClosed
	}
	if operationRoot != e.operation.Intent().Inputs.Root {
		return ErrIdentity
	}
	if err := e.operation.Validate(); err != nil {
		return err
	}
	return e.journal.withOpen(ctx, func() error {
		return e.lease.WithHeld(ctx, func() error {
			if err := e.validateResidentHeld(ctx); err != nil {
				return err
			}
			reader := controlLifecycleReader{controlOwnerReader{controlIntentReader{e.journal.control}}}
			if err := requireResidentLifecycle(ctx, reader, e.journal.pin, e.operation.Intent()); err != nil {
				return err
			}
			// No observations or new errors after native Start: preserve its exact result.
			return start()
		})
	})
}

func (e *ownedResidentEntry) withCurrentIntent(ctx context.Context, reference StartupReference) error {
	return e.journal.withOpen(ctx, func() error {
		return e.lease.WithHeld(ctx, func() error { return e.validateCurrentReference(ctx, reference) })
	})
}

// Must run inside one existing journal borrow and the actual lease guard.
func (e *ownedResidentEntry) validateCurrentReference(ctx context.Context, reference StartupReference) error {
	j := e.journal
	if e.lease == nil || e.lease.path != filepath.Join(j.pin.Root(), instanceLeaseName) {
		return ErrIdentity
	}
	if err := j.checkRoot(ctx); err != nil {
		return err
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return err
	}
	intent, present, err := latestPinnedIntent(ctx, controlIntentReader{j.control}, j.pin)
	if err != nil {
		return err
	}
	if !present {
		return ErrIdentity
	}
	if err = reference.validateIntent(intent); err != nil {
		return err
	}
	return validateCurrentIntent(intent, j.pin, bootID)
}

func (e *ownedResidentEntry) validateResident(ctx context.Context) error {
	return e.journal.withOpen(ctx, func() error {
		return e.lease.WithHeld(ctx, func() error { return e.validateResidentHeld(ctx) })
	})
}

func (e *ownedResidentEntry) validateResidentHeld(ctx context.Context) error {
	if e.endpoint == nil {
		return ErrIdentity
	}
	reference, err := ReferenceStartup(e.operation.Intent())
	if err != nil {
		return err
	}
	if err = e.validateCurrentReference(ctx, reference); err != nil {
		return err
	}
	j := e.journal
	reader := controlOwnerReader{controlIntentReader{j.control}}
	assumption, err := readAssumptionRecord(ctx, reader, j.pin, reference)
	if err != nil {
		return err
	}
	if err = validateSelfAssumption(assumption); err != nil {
		return err
	}
	publication, err := readPublicationRecord(ctx, reader, j.pin, reference)
	if err != nil {
		return err
	}
	if publication.Endpoint != e.endpoint.Identity() {
		return ErrIdentity
	}
	if err = e.endpoint.checkRoot(ctx); err != nil {
		return err
	}
	socket, err := observeEndpointSocket(publication.Endpoint.Path)
	if err != nil {
		return err
	}
	if !e.endpoint.sameSocket(socket) || uint32(socket.Mode)&0o7777 != 0o600 {
		return ErrIdentity
	}
	return ctx.Err()
}
