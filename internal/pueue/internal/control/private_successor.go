package control

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ScopePrivateTask preserves a task's immutable supervisor binding while the
// client controls its proven active successor. The client must still be owned
// exclusively by its preparer; after this one-time scope, observations report
// the durable task binding and commands use the unchanged runtime generation.
func (c *Client) ScopePrivateTask(ctx context.Context, stateRoot string, saved task.SupervisorRef) error {
	if c == nil {
		return ErrConfiguration
	}
	if c.binding != c.runtimeBinding {
		return ErrBinding
	}
	current, found, err := ResolvePrivateSupervisorSuccessor(ctx, stateRoot, saved)
	if err != nil {
		return err
	}
	if !found || !privateFullGenerationMatches(current, c.runtimeBinding) {
		return errors.Join(ErrBinding, ErrPrivateSupervisorMigrationRequired)
	}
	c.binding = saved
	return nil
}

// ResolvePrivateSupervisorSuccessor observes the exact live published binding
// or a completed, connected migration from saved. It never repairs or starts
// a supervisor.
func ResolvePrivateSupervisorSuccessor(ctx context.Context, stateRoot string, saved task.SupervisorRef) (current task.SupervisorRef, found bool, resultErr error) {
	resultErr = WithSupervisorLeaseContext(ctx, stateRoot, func(lockedContext context.Context) error {
		var err error
		current, found, err = ResolvePrivateSupervisorSuccessorWithLock(lockedContext, stateRoot, saved)
		return err
	})
	return current, found, resultErr
}

// ResolvePrivateSupervisorSuccessorWithLock shares the same history decision
// with callers already holding the supervisor lock. The caller must retain a
// shared or exclusive supervisor lease for the entire observation.
func ResolvePrivateSupervisorSuccessorWithLock(ctx context.Context, stateRoot string, saved task.SupervisorRef) (task.SupervisorRef, bool, error) {
	if ctx == nil {
		return task.SupervisorRef{}, false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return task.SupervisorRef{}, false, err
	}
	if !IsPrivateConfig(stateRoot, saved.ConfigPath) {
		return task.SupervisorRef{}, false, ErrConfiguration
	}
	if err := validatePrivateSupervisorRef(saved); err != nil {
		return task.SupervisorRef{}, false, err
	}
	facts, err := loadPrivateGenerationFacts(stateRoot)
	if err != nil {
		return task.SupervisorRef{}, false, err
	}
	if facts.publishedErr != nil || !facts.publishedFound {
		return task.SupervisorRef{}, false, facts.publishedErr
	}
	current := facts.published
	if task.SameSupervisorIdentity(saved, current) {
		phase, phaseErr := facts.phase(current)
		if phaseErr != nil {
			return task.SupervisorRef{}, false, phaseErr
		}
		if phase < privateGenerationObservable {
			return task.SupervisorRef{}, false, ErrPrivateSupervisorMigrationRequired
		}
		return current, true, ctx.Err()
	}
	complete, err := facts.complete(current, false)
	if err != nil {
		return task.SupervisorRef{}, false, err
	}
	if !complete {
		return task.SupervisorRef{}, false, ErrPrivateSupervisorMigrationRequired
	}
	connected, err := privateSupervisorSuccessorConnected(ctx, stateRoot, saved, current)
	if err != nil || !connected {
		return task.SupervisorRef{}, false, err
	}
	return current, true, ctx.Err()
}

// A task-status cache is only a row observation. Revalidate the canonical
// generation inside its SH lease before either reusing or publishing rows.
func (c *Client) verifyPrivateTaskObservationWithLock(ctx context.Context, stateRoot string) error {
	current, found, err := ResolvePrivateSupervisorSuccessorWithLock(ctx, stateRoot, c.binding)
	if err != nil {
		return err
	}
	if !found {
		return ErrPrivateSupervisorMigrationRequired
	}
	if privateBindingsMatch(c.runtimeBinding, current) {
		return ctx.Err()
	}
	matches, err := privateRecoveryRuntimeMatchesPublished(stateRoot, c.runtimeBinding, current)
	if err != nil {
		return err
	}
	if !matches {
		return errors.Join(ErrBinding, ErrPrivateSupervisorMigrationRequired)
	}
	return ctx.Err()
}

// PrivateSupervisorMigrationEdgePublishedWithLock reports whether the exact
// predecessor-to-successor edge and successor binding are durable. Cleanup
// may still be pending. The caller must hold the state-root bootstrap lock.
func PrivateSupervisorMigrationEdgePublishedWithLock(ctx context.Context, stateRoot string, predecessor, successor task.SupervisorRef) (bool, error) {
	return privateSupervisorMigrationEdgePublishedWithLock(ctx, stateRoot, predecessor, successor)
}

// PrivateSupervisorMigrationEdgeCompleteWithLock reports whether the exact
// published edge has also completed every private-generation cleanup phase.
// The caller must hold the state-root bootstrap lock for the whole observation.
func PrivateSupervisorMigrationEdgeCompleteWithLock(ctx context.Context, stateRoot string, predecessor, successor task.SupervisorRef) (bool, error) {
	published, err := privateSupervisorMigrationEdgePublishedWithLock(ctx, stateRoot, predecessor, successor)
	if err != nil || !published {
		return false, err
	}
	complete, err := PrivateSupervisorMigrationComplete(stateRoot, successor)
	if err != nil || !complete {
		return false, err
	}
	return true, ctx.Err()
}

func privateSupervisorMigrationEdgePublishedWithLock(ctx context.Context, stateRoot string, predecessor, successor task.SupervisorRef) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !IsPrivateConfig(stateRoot, predecessor.ConfigPath) || !IsPrivateConfig(stateRoot, successor.ConfigPath) {
		return false, ErrConfiguration
	}
	if err := validatePrivateSupervisorRef(predecessor); err != nil {
		return false, err
	}
	if err := validatePrivateSupervisorRef(successor); err != nil {
		return false, err
	}
	current, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found {
		return false, err
	}
	if current != successor {
		return false, ctx.Err()
	}
	history, found, err := loadPrivateMigrationHistory(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil || !found {
		return false, err
	}
	wantPredecessor := privateDaemonIdentityForBinding(predecessor)
	wantSuccessor := privateDaemonIdentityForBinding(successor)
	edgeFound, err := privateSupervisorMigrationEdgeFound(ctx, history, wantPredecessor, wantSuccessor)
	if err != nil {
		return false, err
	}
	if !edgeFound {
		return false, ctx.Err()
	}
	return true, ctx.Err()
}

func privateSupervisorMigrationEdgeFound(ctx context.Context, history privateMigrationHistory, wantPredecessor, wantSuccessor privateDaemonIdentity) (bool, error) {
	edgeFound := false
	for _, migration := range history.Migrations {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if migration.Legacy != wantPredecessor || migration.Successor != wantSuccessor {
			continue
		}
		if edgeFound {
			return false, ErrBinding
		}
		edgeFound = true
	}
	return edgeFound, nil
}

func privateSupervisorSuccessorConnected(ctx context.Context, stateRoot string, saved, current task.SupervisorRef) (bool, error) {
	history, found, err := loadPrivateMigrationHistory(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil || !found {
		return false, err
	}
	cursor := saved
	for _, migration := range history.Migrations {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if privateLogicalIdentityMatchesBinding(migration.Legacy, cursor) {
			cursor = privateSupervisorBindingForIdentity(cursor, migration.Successor)
		}
	}
	// Logical identity connects predecessor edges, but the completed endpoint
	// must match the final history successor's complete daemon and logical
	// provenance. A same-logical successor with different launch provenance is
	// a different generation and cannot authorize recovery alias retirement.
	return privateDaemonIdentityForBinding(cursor) == privateDaemonIdentityForBinding(current), ctx.Err()
}

// privateCompletedSuccessorHistoryContains proves that the exact completed
// binding was published as a successor in settled migration history. It does
// not walk predecessor edges; callers use privateSupervisorSuccessorConnected
// for that canonical relationship check when a predecessor record exists.
func privateCompletedSuccessorHistoryContains(ctx context.Context, stateRoot string, current task.SupervisorRef) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	history, found, err := loadPrivateMigrationHistory(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil || !found {
		return false, err
	}
	want := privateDaemonIdentityForBinding(current)
	for _, migration := range history.Migrations {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if migration.Successor == want {
			return true, nil
		}
	}
	return false, ctx.Err()
}
