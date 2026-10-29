package verifiedexec

// Prepare publishes the pool identity marker once, and the guard every other
// operation applies before it changes anything. See pool_identity.go for the
// marker and pool_identity_fs.go for the descriptor operations.
//
// Prepare runs under the exclusive gate from before the stage exists until the
// stage is gone, so no other publisher of a marker is alive while it holds the
// gate, and a recognized stage seen under the gate is its own or abandoned:
//
//	verify the layout and pin the root; strict-read any marker
//	absent:  create a stage (0600), write the canonical record, flush the stage,
//	         link it to the marker name without replacing anything,
//	         flush the root, strict-read the marker back
//	present: (a winner, or the result of an earlier crash) flush the marker and
//	         the root again, then strict-read it back
//	acknowledge the identity, then remove the stage(s) and flush the root

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Prepare creates and verifies the pool layout and returns the pool's identity,
// publishing the identity marker first if the root has none. It creates no
// images and no views. It is idempotent: any number of processes may Prepare
// the same root and all receive the same identity. An id is acknowledged only
// after the marker and its directory entry are durable and the marker reads
// back strictly canonical.
//
// Only an unbound pool may Prepare; a pool made by NewPoolWithIdentity refuses,
// so an identity is never reinitialized. A root that is an alias, not private,
// on a noexec filesystem (ErrPoolNoExec) or on a split device is refused before
// anything is published through it. A failed barrier after the marker was linked
// returns task.ErrUncertainDurability without acknowledging; a retry finds the
// marker and repairs the barriers. Failing to remove a finished or abandoned
// stage afterwards is logged and retried by the next Prepare or reclaim, and
// does not change the returned identity. ctx must be non-nil.
func (p *Pool) Prepare(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", errNilContext
	}
	if p.id != "" {
		return "", errPoolPrepareGuarded
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The gate opener creates the root's missing ancestors, so an alias is
	// refused before anything is created through it.
	if err := p.requireCanonicalAncestry(); err != nil {
		return "", err
	}
	gate, err := p.openGate(ctx)
	if err != nil {
		return "", err
	}
	id, prepareErr := p.prepareUnderGate(ctx, gate)
	if err = errors.Join(prepareErr, gate.Close()); err != nil {
		return "", err
	}
	return id, nil
}

func (p *Pool) prepareUnderGate(ctx context.Context, gate *taskdir.LockFile) (string, error) {
	if err := gate.LockEX(ctx); err != nil {
		return "", err
	}
	id, err := p.establishIdentity()
	return id, errors.Join(err, gate.Unlock())
}

// establishIdentity runs the protocol with the exclusive gate held.
func (p *Pool) establishIdentity() (string, error) {
	root, err := p.pinRoot()
	if err != nil {
		return "", err
	}
	id, err := root.establish()
	if err == nil {
		p.cleanIdentityStages(root)
	}
	return id, errors.Join(err, root.Close())
}

// cleanIdentityStages is the best-effort part of Prepare, after the identity is
// durable: a failure is logged in bounded form and retried by the next Prepare.
func (p *Pool) cleanIdentityStages(root *pinnedRoot) {
	if _, err := root.sweepStages(); err != nil {
		p.logf("custody pool %s: identity stage cleanup failed: %s", p.root, bounded(err.Error()))
	}
}

// establish returns the identity of a root with the gate held: the existing
// marker after it was flushed and read back, or a newly published one.
func (r *pinnedRoot) establish() (string, error) {
	id, err := r.readIdentity()
	switch {
	case err == nil:
		return r.settle(id, false)
	case errors.Is(err, fs.ErrNotExist):
		return r.publish()
	default:
		return "", err
	}
}

func (r *pinnedRoot) publish() (string, error) {
	id, err := newPoolID()
	if err != nil {
		return "", err
	}
	stage, err := r.writeStage(id)
	if err != nil {
		return "", err
	}
	switch err = r.linkMarker(stage); {
	case err == nil:
		r.pool.at(pointMarkerLinked, "")
		return r.settle(id, true)
	case errors.Is(err, fs.ErrExist):
		return r.adoptWinner()
	default:
		return "", err
	}
}

// adoptWinner is the create-once loser's path: the marker another publisher won
// is never overwritten, and is returned only after it is flushed and verified.
func (r *pinnedRoot) adoptWinner() (string, error) {
	id, err := r.readIdentity()
	if err != nil {
		return "", err
	}
	return r.settle(id, false)
}

// settle flushes the marker and its directory entry and then reads it back
// strictly; the identity may be acknowledged only after it returns. linked
// reports that this caller itself made the marker visible, so a barrier failure
// leaves a published marker without a durability proof.
func (r *pinnedRoot) settle(id string, linked bool) (string, error) {
	if err := r.flushIdentity(); err != nil {
		if linked {
			return "", fmt.Errorf("%w: %w", task.ErrUncertainDurability, err)
		}
		return "", err
	}
	confirmed, err := r.readIdentity()
	if err != nil {
		return "", err
	}
	if confirmed != id {
		return "", fmt.Errorf("%w: the marker changed while it was being published", errPoolIdentityMismatch)
	}
	return confirmed, nil
}

// requireIdentity is the guard of every operation that reads or changes the pool
// on behalf of an owner. It proves, without creating or changing anything, that
// the recorded canonical private root holds the strictly canonical marker of the
// identity this pool is bound to. A missing root or marker, a mismatch, an
// alias, an unsafe owner or mode, a corrupt record and an unreadable root all
// leave the pool unresolved with the concrete cause: the caller must not report
// a release, reselect an identity or repair the root. An unbound pool is refused.
func (p *Pool) requireIdentity() error {
	if p.id == "" {
		return errPoolUnbound
	}
	if err := p.proveIdentity(); err != nil {
		return fmt.Errorf("%w: %w", errPoolIdentityUnverified, err)
	}
	p.at(pointIdentityChecked, "")
	return nil
}

func (p *Pool) proveIdentity() (resultErr error) {
	if err := p.requireCanonicalPrivateRoot(); err != nil {
		return err
	}
	root, err := p.pinRoot()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	id, err := root.readIdentity()
	if err != nil {
		return err
	}
	if id != p.id {
		return fmt.Errorf("%w: %s", errPoolIdentityMismatch, p.root)
	}
	return nil
}
