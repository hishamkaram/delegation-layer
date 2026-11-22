package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestUpgradeAcceptedStartObservationRequiresBootstrapEX(t *testing.T) {
	f, pin, _ := upgradeMigrationStorageFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	before := snapshotPrivateSuccessorTestState(t, f.root)
	observed, accepted, err := ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, "")
	if !errors.Is(err, ErrBinding) || accepted || observed != (PrivateUpgradeObservation{}) {
		t.Fatalf("unlocked observation was accepted: observed=%+v accepted=%t err=%v", observed, accepted, err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.root, before)
}

// These are canonical storage facts, not evidence of native Start or closure.
// In particular, accepted unknown birth must never be reduced to a PID guess.
func TestUpgradeAcceptedStartObservationUsesCanonicalMigration(t *testing.T) {
	for _, test := range []struct {
		name     string
		accepted bool
		birth    bool
	}{
		{name: "before target Start"},
		{name: "accepted unknown birth", accepted: true},
		{name: "accepted recorded birth", accepted: true, birth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exerciseUpgradeAcceptedStartObservation(t, test.accepted, test.birth)
		})
	}
}

func exerciseUpgradeAcceptedStartObservation(t *testing.T, wantAccepted, withBirth bool) {
	t.Helper()
	f, pin, pending := upgradeMigrationStorageFixture(t)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		updated := pending
		updated.UpgradeGeneration.AcceptedStart = wantAccepted
		if withBirth {
			updated.UpgradeGeneration.SuccessorBootID = pending.UpgradeGeneration.LegacyBootID
			updated.UpgradeGeneration.SuccessorPID = 124
			updated.UpgradeGeneration.SuccessorProcessStart = "recorded-successor"
		}
		if updated != pending {
			if err := replacePrivateMigrationRecordContext(ctx, f.base, pending, updated, f.client.options.PrivateGenerationGuard); err != nil {
				return err
			}
		}
		before := snapshotPrivateSuccessorTestState(t, f.root)
		for _, operation := range []UpgradeOperationID{"", pending.UpgradeGeneration.OperationID} {
			observed, accepted, err := ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, operation)
			if err != nil || accepted != wantAccepted || observed.OperationID != pending.UpgradeGeneration.OperationID || observed.Completed {
				t.Fatalf("canonical accepted Start changed: observed=%+v accepted=%t want=%t err=%v", observed, accepted, wantAccepted, err)
			}
		}
		assertPrivateSuccessorTestStateUnchanged(t, f.root, before)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeAcceptedStartObservationFailsClosed(t *testing.T) {
	for _, fault := range []string{"corrupt", "not regular", "missing migrating record", "different operation", "different target"} {
		t.Run(fault, func(t *testing.T) {
			f, pin, pending := upgradeMigrationStorageFixture(t)
			damageUpgradeRefreshFixture(t, privateMigrationRecordPath(f.base), pending, fault)
			before := snapshotPrivateSuccessorTestState(t, f.root)
			err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
				_, accepted, readErr := ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, "")
				if readErr == nil || accepted {
					t.Fatalf("canonical fault became a safe negative fact: accepted=%t err=%v", accepted, readErr)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			assertPrivateSuccessorTestStateUnchanged(t, f.root, before)
		})
	}
}

func damageUpgradeRefreshFixture(t *testing.T, path string, pending privateMigrationRecord, fault string) {
	t.Helper()
	switch fault {
	case "corrupt":
		writeUpgradeRefreshFixture(t, path, []byte("{"))
	case "not regular", "missing migrating record":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if fault == "not regular" {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	case "different operation", "different target":
		if fault == "different operation" {
			pending.UpgradeGeneration.OperationID = UpgradeOperationID("upgrade-v1:" + strings.Repeat("f", 32))
		} else {
			pending.UpgradeGeneration.Target.DaemonExecutable += "-other"
			pending.Successor = privateDaemonIdentityForBinding(pending.UpgradeGeneration.Target)
		}
		data, err := task.MarshalCanonical(pending)
		if err != nil {
			t.Fatal(err)
		}
		writeUpgradeRefreshFixture(t, path, data)
	}
}

func TestUpgradeAcceptedStartObservationWithoutOperationIsReadOnly(t *testing.T) {
	f, _ := runtimeRestartJournalFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		observed, accepted, readErr := ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, "")
		if readErr != nil || accepted || observed != (PrivateUpgradeObservation{}) {
			t.Fatalf("ordinary state became an upgrade: observed=%+v accepted=%t err=%v", observed, accepted, readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.root, before)
}

func TestUpgradeAcceptedStartCompletedReplayDoesNotReadCurrentMigration(t *testing.T) {
	f, pin, pending := upgradeMigrationStorageFixture(t)
	completed := pending
	completed.UpgradeGeneration.AcceptedStart = true
	completed.UpgradeGeneration.SuccessorBootID = pending.UpgradeGeneration.LegacyBootID
	completed.UpgradeGeneration.SuccessorPID = 124
	completed.UpgradeGeneration.SuccessorProcessStart = "recorded-successor"
	completed.UpgradeGeneration.Completed = true
	if err := appendPrivateMigrationHistory(f.base, completed); err != nil {
		t.Fatal(err)
	}
	bootstrap, found, err := readPrivateBootstrapRecord(f.root)
	if err != nil || !found || bootstrap.DrainV1 == nil {
		t.Fatalf("canonical fixture drain unavailable: found=%t err=%v", found, err)
	}
	current := *bootstrap.DrainV1
	current.OperationID = UpgradeOperationID("upgrade-v1:" + strings.Repeat("e", 32))
	current.Phase = UpgradeDraining
	current.CompletionAuthorization = privateUpgradeOperationRef{}
	current.SelectedPair = privateUpgradePair{}
	current.MigrationReference = privateUpgradeOperationRef{}
	bootstrap.DrainV1 = &current
	data, err := task.MarshalCanonical(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	writeUpgradeRefreshFixture(t, privateBootstrapRecordPath(f.root), data)
	writeUpgradeRefreshFixture(t, filepath.Join(f.base, "migration.json"), []byte("unreadable current B migration"))
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		observed, accepted, readErr := ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, completed.UpgradeGeneration.OperationID)
		if readErr != nil || accepted || !observed.Completed || observed.OperationID != completed.UpgradeGeneration.OperationID {
			t.Fatalf("completed A read current B: observed=%+v accepted=%t err=%v", observed, accepted, readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.root, before)
}

func TestUpgradeAcceptedStartObservationAllowsRecordedCompletionTail(t *testing.T) {
	exerciseUpgradeRefreshCompletedPublication(t, "migration_removed")
}

func TestUpgradeAcceptedStartObservationAllowsRetainedCompletedMigration(t *testing.T) {
	exerciseUpgradeRefreshCompletedPublication(t, "history")
}

func exerciseUpgradeRefreshCompletedPublication(t *testing.T, phase string) {
	t.Helper()
	f, pin, completed := upgradeRefreshCompletedPublicationFixture(t, phase)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		complete, readErr := privateMigrationCompletionRecorded(f.root, f.base, completed.UpgradeGeneration.Target, completed)
		if readErr != nil || !complete {
			t.Fatalf("recorded completion fixture lacks exact published completion: complete=%t err=%v", complete, readErr)
		}
		assertUpgradeRefreshRecordedTailReadOnly(t, ctx, pin, completed.UpgradeGeneration.OperationID)
		// Only the existing mutation owner may clear the retained drain. This
		// storage fixture has no installed target and must not select or Start.
		options := f.client.options
		options.DaemonWaitOwner = &upgradeNativeWaitOwner{}
		client, observed, completeErr := CompletePrivateUpgradeWithLock(ctx, pin, completed.UpgradeGeneration.OperationID, func(context.Context) (string, string, error) {
			t.Error("recorded completion tail reselected a target")
			return "", "", ErrConfiguration
		}, options)
		if client != nil {
			completeErr = errors.Join(completeErr, client.CloseAfterPending(completeErr))
		}
		if completeErr != nil || client != nil || !observed.Completed || observed.OperationID != completed.UpgradeGeneration.OperationID {
			t.Errorf("existing completion owner did not finish the exact tail: observed=%+v client=%t err=%v", observed, client != nil, completeErr)
		}
		return completeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	assertUpgradePublicationWinner(t, f, pin, completed)
}

func assertUpgradeRefreshRecordedTailReadOnly(t *testing.T, ctx context.Context, pin taskdir.RootPin, operation UpgradeOperationID) {
	t.Helper()
	before := snapshotPrivateSuccessorTestState(t, pin.Root())
	for _, selected := range []UpgradeOperationID{"", operation} {
		observed, accepted, readErr := ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, selected)
		if readErr != nil || accepted || observed.Completed || observed.Phase != UpgradeMigrating || observed.OperationID != operation {
			t.Errorf("valid completion tail blocked canonical cleanup or synthesized completion: operation=%q observed=%+v accepted=%t err=%v", selected, observed, accepted, readErr)
		}
	}
	assertPrivateSuccessorTestStateUnchanged(t, pin.Root(), before)
}

func upgradeRefreshRecordedCompletionFixture(t *testing.T) (generationGuardFixture, taskdir.RootPin, privateMigrationRecord) {
	t.Helper()
	return upgradeRefreshCompletedPublicationFixture(t, "migration_removed")
}

// These are the production commit's history-written/drain-held interruption
// points. Process births remain fixture data, not native proof.
func upgradeRefreshCompletedPublicationFixture(t *testing.T, phase string) (generationGuardFixture, taskdir.RootPin, privateMigrationRecord) {
	t.Helper()
	f, pin, pending := upgradeMigrationStorageFixture(t)
	completed := pending
	completed.UpgradeGeneration.AcceptedStart = true
	completed.UpgradeGeneration.SuccessorBootID = pending.UpgradeGeneration.LegacyBootID
	completed.UpgradeGeneration.SuccessorPID = 124
	completed.UpgradeGeneration.SuccessorProcessStart = "recorded-successor"
	completed.UpgradeGeneration.Completed = true
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		if err := replacePrivateMigrationRecordContext(ctx, f.base, pending, completed, f.client.options.PrivateGenerationGuard); err != nil {
			return err
		}
		return stageUpgradePublicationInterruption(f.root, f.base, completed, phase)
	})
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := loadPrivateMigrationRecord(f.base)
	if err != nil || found != (phase == "history") || found && current != completed {
		t.Fatalf("completed publication fixture has wrong retained migration: found=%t err=%v", found, err)
	}
	return f, pin, completed
}

func TestUpgradeAcceptedStartRetainedCompletionRejectsIncompleteEvidence(t *testing.T) {
	for _, fault := range []string{"missing history", "different history", "corrupt history", "binding", "daemon", "pending publication"} {
		t.Run(fault, func(t *testing.T) {
			f, pin, completed := upgradeRefreshCompletedPublicationFixture(t, "history")
			damageUpgradeRefreshRetainedCompletion(t, f, completed, fault)
			complete, err := privateMigrationCompletionRecorded(f.root, f.base, completed.UpgradeGeneration.Target, completed)
			if complete || fault == "corrupt history" && err == nil || fault != "corrupt history" && err != nil {
				t.Fatalf("retained completion fixture has wrong missing-evidence cell: complete=%t err=%v", complete, err)
			}
			assertUpgradeRefreshRecordedTailFault(t, pin, completed.UpgradeGeneration.OperationID)
		})
	}
}

func damageUpgradeRefreshRetainedCompletion(t *testing.T, f generationGuardFixture, completed privateMigrationRecord, fault string) {
	t.Helper()
	switch fault {
	case "missing history":
		if err := os.Remove(privateMigrationHistoryPath(f.base)); err != nil {
			t.Fatal(err)
		}
	case "different history":
		// Keep the same OP and target but require the exact completed migration,
		// including its accepted successor birth, to match durable history.
		different := completed
		different.UpgradeGeneration.SuccessorPID++
		writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(f.base), privateMigrationHistory{SchemaVersion: task.SchemaVersion, Migrations: []privateMigrationRecord{different}})
	case "corrupt history":
		writeUpgradeRefreshFixture(t, privateMigrationHistoryPath(f.base), []byte("{"))
	case "binding":
		writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(f.root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: completed.UpgradeGeneration.Predecessor})
	case "daemon":
		writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(f.base), privateDaemonIdentityForBinding(completed.UpgradeGeneration.Predecessor))
	case "pending publication":
		writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(f.root), privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: completed.UpgradeGeneration.Target})
	}
}

func TestUpgradeAcceptedStartRecordedTailRejectsBrokenCorrelation(t *testing.T) {
	for _, field := range []string{"operation", "root", "predecessor", "mode", "target"} {
		t.Run(field, func(t *testing.T) {
			f, pin, completed := upgradeRefreshRecordedCompletionFixture(t)
			corrupt := changeUpgradeRefreshCompletion(completed, field)
			writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(f.base), privateMigrationHistory{SchemaVersion: task.SchemaVersion, Migrations: []privateMigrationRecord{corrupt}})
			if field == "target" {
				// Keep the published target internally consistent with history:
				// the missing protection must be linkage to the retained Drain.
				writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(f.root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: corrupt.UpgradeGeneration.Target})
				writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(f.base), privateDaemonIdentityForBinding(corrupt.UpgradeGeneration.Target))
			}
			complete, err := privateMigrationCompletionRecorded(f.root, f.base, corrupt.UpgradeGeneration.Target, corrupt)
			if err != nil || !complete {
				t.Fatalf("correlation fixture was not otherwise a published completion: complete=%t err=%v", complete, err)
			}
			assertUpgradeRefreshRecordedTailFault(t, pin, completed.UpgradeGeneration.OperationID)
		})
	}
}

func changeUpgradeRefreshCompletion(record privateMigrationRecord, field string) privateMigrationRecord {
	switch field {
	case "operation":
		record.UpgradeGeneration.OperationID = UpgradeOperationID("upgrade-v1:" + strings.Repeat("d", 32))
	case "root":
		record.UpgradeGeneration.Root.ID = strings.Repeat("d", 32)
	case "predecessor":
		record.UpgradeGeneration.Predecessor.ClientExecutable += "-other"
	case "mode":
		record.UpgradeGeneration.Mode = UpgradeReactive
	case "target":
		record.UpgradeGeneration.Target.DaemonExecutable += "-other"
		record.Successor = privateDaemonIdentityForBinding(record.UpgradeGeneration.Target)
	}
	return record
}

func TestUpgradeAcceptedStartRecordedTailRejectsIncompletePublication(t *testing.T) {
	for _, fault := range []string{"binding", "daemon", "pending publication"} {
		t.Run(fault, func(t *testing.T) {
			f, pin, completed := upgradeRefreshRecordedCompletionFixture(t)
			switch fault {
			case "binding":
				writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(f.root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: completed.UpgradeGeneration.Predecessor})
			case "daemon":
				writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(f.base), privateDaemonIdentityForBinding(completed.UpgradeGeneration.Predecessor))
			case "pending publication":
				writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(f.root), privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: completed.UpgradeGeneration.Target})
			}
			complete, err := privateMigrationCompletionRecorded(f.root, f.base, completed.UpgradeGeneration.Target, completed)
			if err != nil || complete {
				t.Fatalf("publication fixture did not provide the false,nil completion cell: complete=%t err=%v", complete, err)
			}
			assertUpgradeRefreshRecordedTailFault(t, pin, completed.UpgradeGeneration.OperationID)
		})
	}
}

func TestUpgradeAcceptedStartRecordedTailRejectsUnreadableHistory(t *testing.T) {
	for _, fault := range []string{"corrupt", "not regular"} {
		t.Run(fault, func(t *testing.T) {
			f, pin, completed := upgradeRefreshRecordedCompletionFixture(t)
			path := privateMigrationHistoryPath(f.base)
			if fault == "corrupt" {
				writeUpgradeRefreshFixture(t, path, []byte("{"))
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			assertUpgradeRefreshRecordedTailFault(t, pin, completed.UpgradeGeneration.OperationID)
		})
	}
}

func assertUpgradeRefreshRecordedTailFault(t *testing.T, pin taskdir.RootPin, operation UpgradeOperationID) {
	t.Helper()
	before := snapshotPrivateSuccessorTestState(t, pin.Root())
	err := WithExistingBootstrapLockContext(t.Context(), pin.Root(), func(ctx context.Context) error {
		for _, selected := range []UpgradeOperationID{"", operation} {
			observed, accepted, readErr := ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, selected)
			if readErr == nil || accepted || observed.Completed || observed.OperationID != operation {
				t.Errorf("invalid completion tail was not rejected with exact pending operation: observed=%+v accepted=%t err=%v", observed, accepted, readErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, pin.Root(), before)
}

func writeUpgradeRefreshFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
