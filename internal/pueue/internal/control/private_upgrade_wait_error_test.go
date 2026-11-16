package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The source owner retains both the pending meaning and independent faults.
// Migration may not add another authority/error leaf to an upgrade Wait result.
type upgradeWaitSourceError struct {
	identity *int
	cause    error
}

func (*upgradeWaitSourceError) Error() string              { return "fixture Wait observation" }
func (e *upgradeWaitSourceError) Unwrap() error            { return e.cause }
func (e *upgradeWaitSourceError) waitSourceIdentity() *int { return e.identity }

func upgradeUnclosedMigrationFixture(t *testing.T, dispatched bool) (generationGuardFixture, privateMigrationRecord) {
	t.Helper()
	f, pin, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	target := f.client.Binding()
	target.DaemonExecutable = filepath.Join(filepath.Dir(target.DaemonExecutable), "next-pueued")
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(f.client.Binding()), Successor: privateDaemonIdentityForBinding(target),
		LegacyPID: 123, LegacyProcessStart: "recorded-predecessor", LegacyShutdownRequested: true, LegacyShutdownDispatched: dispatched,
		UpgradeGeneration: migration.UpgradeGeneration{OperationID: accepted.OperationID, Root: upgradeRootSubject(pin), Predecessor: f.client.Binding(), Mode: UpgradeOperationRequired, Target: target, LegacyBootID: boot},
	}
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		drain, found, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil || !found {
			return errors.Join(ErrBinding, readErr)
		}
		authorized := drain
		authorized.Phase = UpgradeAuthorized
		authorized.CompletionAuthorization = privateUpgradeOperationRef{OperationID: accepted.OperationID}
		authorized.SelectedPair = privateUpgradePair{ClientExecutable: target.ClientExecutable, DaemonExecutable: target.DaemonExecutable}
		if writeErr := replacePrivateUpgrade(ctx, pin, drain, authorized, f.client.options); writeErr != nil {
			return writeErr
		}
		if writeErr := publishPrivateMigrationStateContext(ctx, f.base, pending, f.client.options.PrivateGenerationGuard); writeErr != nil {
			return writeErr
		}
		_, linkErr := linkPrivateUpgradeMigration(ctx, pin, authorized, pending, f.client.options)
		return linkErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, pending
}

func TestUpgradePredecessorWaitPreservesSourceErrorAtBothShutdownPhases(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		name := "requested"
		if dispatched {
			name = "dispatched"
		}
		t.Run(name, func(t *testing.T) { testUpgradeWaitSourceForwarding(t, dispatched) })
	}
}

func testUpgradeWaitSourceForwarding(t *testing.T, dispatched bool) {
	t.Helper()
	f, pending := upgradeUnclosedMigrationFixture(t, dispatched)
	for _, cause := range []error{ErrPrivateUpgradePending, errors.Join(ErrPrivateUpgradePending, os.ErrPermission, context.Canceled, os.ErrClosed)} {
		source := &upgradeWaitSourceError{identity: new(int), cause: cause}
		f.client.options.DaemonWaitOwner = upgradeSettlementFixture{err: source}
		f.client.options.processObserver = func(int) (privateProcessObservation, error) { return privateProcessObservation{}, nil }
		before := snapshotExecutionTargetTree(t, f.base)
		err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
			var observed privateMigrationRecord
			var waitErr error
			if dispatched {
				observed, waitErr = observeDispatchedPrivateMigrationShutdown(ctx, f.base, f.client.Binding(), pending, f.client.options)
			} else {
				observed, _, waitErr = dispatchPrivateMigrationShutdown(ctx, f.base, f.client, pending)
			}
			if observed != pending || observed.LegacyStopped || observed.UpgradeGeneration.AcceptedStart {
				t.Fatalf("unresolved Wait advanced canonical migration: %+v", observed)
			}
			return waitErr
		})
		forwarded, original := err.(interface{ waitSourceIdentity() *int })
		if !original || forwarded.waitSourceIdentity() != source.identity || errors.Is(err, ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, cause) {
			t.Fatalf("upgrade Wait source error was changed: err=%v", err)
		}
		assertExecutionTargetTreeUnchanged(t, f.base, before)
	}
}
