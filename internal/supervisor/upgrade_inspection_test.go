package supervisor

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type upgradeInspectionGuard func(context.Context, string, func() error) error

type upgradeInspectionStartGuard func(context.Context, string, func() error) error

func (g upgradeInspectionStartGuard) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return g(ctx, root, start)
}

func (g upgradeInspectionGuard) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	return g(ctx, root, commit)
}

type upgradeInspectionHistoryRecord struct {
	parentMigrationRecordFixture
	LegacyProcessStart       string                             `json:"legacy_process_start"`
	LegacyShutdownRequested  bool                               `json:"legacy_shutdown_requested"`
	LegacyShutdownDispatched bool                               `json:"legacy_shutdown_dispatched"`
	UpgradeGeneration        upgradeInspectionHistoryGeneration `json:"upgrade_generation"`
}

type upgradeInspectionHistoryGeneration struct {
	OperationID           string                `json:"operation_id"`
	Root                  upgradeInspectionRoot `json:"root"`
	Predecessor           task.SupervisorRef    `json:"predecessor"`
	Mode                  string                `json:"mode"`
	Target                task.SupervisorRef    `json:"target"`
	LegacyBootID          string                `json:"legacy_boot_id"`
	AcceptedStart         bool                  `json:"accepted_start"`
	SuccessorBootID       string                `json:"successor_boot_id"`
	SuccessorPID          int                   `json:"successor_pid"`
	SuccessorProcessStart string                `json:"successor_process_start"`
	Completed             bool                  `json:"completed"`
}

type upgradeInspectionRoot struct {
	Path   string `json:"path"`
	ID     string `json:"id"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func TestUpgradeInspectionHistoricalSameTargetAThenBDoesNotBorrowBirth(t *testing.T) {
	f, client := unpreparedMigrationFixture(t)
	a, b := writeSameTargetUpgradeInspectionHistory(t, f)
	assertHistoricalUpgradeInspectionReadOnly(t, f, a, b)
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(f.store.Root, ".supervisor", "migration-history.json")
	privateBefore, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	err = pueue.WithExistingBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		return CompleteUpgradeInspectionWithLock(ctx, f.store, pin, pueue.UpgradeOperationID(b.UpgradeGeneration.OperationID), client, upgradeInspectionGuard(allowedUpgradeInspectionCommit))
	})
	if err != nil {
		t.Fatal(err)
	}
	privateAfter, err := os.ReadFile(history)
	if err != nil || !bytes.Equal(privateBefore, privateAfter) {
		t.Fatalf("B inspection cleanup changed private history: %v", err)
	}
	present, matches, err := inspection.GroupHeadMatchesBinding(t.Context(), f.store, f.target)
	if err != nil || !present || !matches {
		t.Fatalf("current B did not complete its derived edge: %v", err)
	}
}

func assertHistoricalUpgradeInspectionReadOnly(t *testing.T, f parentMigrationFixture, a, b upgradeInspectionHistoryRecord) {
	t.Helper()
	before := parentMigrationTree(t, f.store.Root)
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = pueue.WithExistingBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		observedA, _, observeErr := pueue.ObservePrivateUpgrade(ctx, pin, pueue.UpgradeOperationID(a.UpgradeGeneration.OperationID))
		if observeErr != nil {
			return observeErr
		}
		observedB, _, observeErr := pueue.ObservePrivateUpgrade(ctx, pin, pueue.UpgradeOperationID(b.UpgradeGeneration.OperationID))
		if observeErr != nil {
			return observeErr
		}
		if observedA.Target != observedB.Target || observedA.OperationID == observedB.OperationID || a.UpgradeGeneration.SuccessorPID == b.UpgradeGeneration.SuccessorPID || a.UpgradeGeneration.SuccessorProcessStart == b.UpgradeGeneration.SuccessorProcessStart {
			t.Fatal("fixture does not distinguish same-target native births")
		}
		_, eligible, targetErr := pueue.PrivateUpgradeInspectionTargetWithLock(ctx, pin, observedA.OperationID)
		if targetErr != nil || eligible {
			t.Fatalf("historical A borrowed B's native generation: eligible=%t err=%v", eligible, targetErr)
		}
		return nil
	})
	if err != nil || !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
		t.Fatalf("historical A changed current B: %v", err)
	}
}

// Exact historical data, not real process closure. The two records deliberately
// share a full target but retain different OP and successor birth identities.
func writeSameTargetUpgradeInspectionHistory(t *testing.T, f parentMigrationFixture) (upgradeInspectionHistoryRecord, upgradeInspectionHistoryRecord) {
	t.Helper()
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	a := upgradeInspectionHistoryRecord{
		parentMigrationRecordFixture: parentMigrationRecordFixture{task.SchemaVersion, parentMigrationIdentity(f.legacy), parentMigrationIdentity(f.target), 12345, true},
		LegacyProcessStart:           "fixture-old", LegacyShutdownRequested: true, LegacyShutdownDispatched: true,
		UpgradeGeneration: upgradeInspectionHistoryGeneration{OperationID: "upgrade-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Root: upgradeInspectionRoot{pin.Root(), pin.RootID(), pin.Device(), pin.Inode()}, Predecessor: f.legacy, Mode: "operation_required", Target: f.target, LegacyBootID: boot, AcceptedStart: true, SuccessorBootID: boot, SuccessorPID: 10001, SuccessorProcessStart: "fixture-A", Completed: true},
	}
	b := a
	b.Legacy = parentMigrationIdentity(f.target)
	b.LegacyPID, b.LegacyProcessStart = a.UpgradeGeneration.SuccessorPID, a.UpgradeGeneration.SuccessorProcessStart
	b.UpgradeGeneration.Predecessor = f.target
	b.UpgradeGeneration.OperationID = "upgrade-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	b.UpgradeGeneration.SuccessorPID, b.UpgradeGeneration.SuccessorProcessStart = 10002, "fixture-B"
	writeContinuationFixtureRecord(t, filepath.Join(f.store.Root, ".supervisor", "migration-history.json"), struct {
		SchemaVersion int                              `json:"schema_version"`
		Migrations    []upgradeInspectionHistoryRecord `json:"migrations"`
	}{task.SchemaVersion, []upgradeInspectionHistoryRecord{a, b}})
	return a, b
}

func allowedUpgradeInspectionCommit(_ context.Context, _ string, commit func() error) error {
	return commit()
}

// Canonical historical generation DATA plus finite, joined status commands.
// Actual daemon birth/Wait and full Gateway wiring are covered by the App case.
func TestUpgradeInspectionCompletesOnlyDerivedPreparedOrFreshEdge(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "prepared"}[prepared], func(t *testing.T) {
			testUpgradeInspectionDerivedEdge(t, prepared)
		})
	}
}

func testUpgradeInspectionDerivedEdge(t *testing.T, prepared bool) {
	t.Helper()
	f, client := unpreparedMigrationFixture(t)
	if prepared {
		if err := inspection.PrepareGroupMigration(t.Context(), f.store, InspectionGroup(f.store.RootID, f.legacy), InspectionGroup(f.store.RootID, f.target)); err != nil {
			t.Fatal(err)
		}
	}
	history := filepath.Join(f.store.Root, ".supervisor", "migration-history.json")
	before, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = runUpgradeInspection(t, f, client, upgradeInspectionGuard(allowedUpgradeInspectionCommit)); err != nil {
			t.Fatal(err)
		}
	}
	present, matches, err := inspection.GroupHeadMatchesBinding(t.Context(), f.store, f.target)
	if err != nil || !present || !matches {
		t.Fatalf("completed edge not current: present=%t matches=%t err=%v", present, matches, err)
	}
	after, err := os.ReadFile(history)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("inspection completion rewrote private history: %v", err)
	}
}

func runUpgradeInspection(t *testing.T, f parentMigrationFixture, client *pueue.Client, guard pueue.PrivateGenerationGuard) error {
	t.Helper()
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return pueue.WithExistingBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		return CompleteUpgradeInspectionWithLock(ctx, f.store, pin, "", client, guard)
	})
}

func TestUpgradeInspectionRefusesUnsafeEvidenceBeforeWrites(t *testing.T) {
	for _, partition := range []string{"active foreign queue", "ended unproven inspection", "malformed root resource", "conflicting proposal", "recovery alias", "missing authority", "missing resident guard", "daemon exited after probe", "revoked commit"} {
		t.Run(partition, func(t *testing.T) {
			f, client := unpreparedMigrationFixture(t)
			guard := pueue.PrivateGenerationGuard(upgradeInspectionGuard(allowedUpgradeInspectionCommit))
			guard, client = configureUpgradeInspectionRefusal(t, f, client, guard, partition)
			before := parentMigrationTree(t, f.store.Root)
			err := runUpgradeInspection(t, f, client, guard)
			if err == nil || !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
				t.Fatalf("unsafe inspection evidence acquired writes: partition=%s err=%v", partition, err)
			}
			if partition == "active foreign queue" && !MaintenanceBusy(err) {
				t.Fatalf("active work lost pending classification: %v", err)
			}
			if partition != "active foreign queue" && partition != "ended unproven inspection" && MaintenanceBusy(err) {
				t.Fatalf("hard evidence fault became pending: %v", err)
			}
		})
	}
}

func configureUpgradeInspectionRefusal(t *testing.T, f parentMigrationFixture, client *pueue.Client, guard pueue.PrivateGenerationGuard, partition string) (pueue.PrivateGenerationGuard, *pueue.Client) {
	t.Helper()
	switch partition {
	case "active foreign queue", "ended unproven inspection", "malformed root resource":
		configureFreshInspectionBlocker(t, f, partition)
	case "conflicting proposal":
		other := f.target
		other.ConfigDigest = task.ComputeSHA256([]byte("different unaccepted target"))
		if err := inspection.PrepareGroupMigration(t.Context(), f.store, InspectionGroup(f.store.RootID, f.legacy), InspectionGroup(f.store.RootID, other)); err != nil {
			t.Fatal(err)
		}
	case "recovery alias":
		writeContinuationAlias(t, f, f.legacy)
	case "missing authority":
		if err := os.Remove(filepath.Join(f.store.Root, ".supervisor", "daemon.identity.json")); err != nil {
			t.Fatal(err)
		}
	case "missing resident guard":
		guard = nil
	case "daemon exited after probe":
		if _, err := client.Snapshot(t.Context()); err != nil {
			t.Fatalf("initial endpoint probe did not succeed: %v", err)
		}
		// The caller's bound client remains, but the next actual control command
		// observes failure. No Start owner is lent to this completion path.
		options := f.options
		options.Environment = append(options.Environment, "CONTROL_FIXTURE_UNAVAILABLE=1")
		options.DaemonStartGuard = upgradeInspectionStartGuard(func(context.Context, string, func() error) error {
			t.Error("derived inspection completion requested a fresh daemon Start")
			return os.ErrPermission
		})
		var err error
		client, err = pueue.NewClient(f.target, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	case "revoked commit":
		guard = upgradeInspectionGuard(func(context.Context, string, func() error) error {
			return errors.Join(context.Canceled, os.ErrPermission)
		})
	default:
		t.Fatal("unknown inspection refusal partition")
	}
	return guard, client
}

func TestUpgradeInspectionNoHeadIsReadOnlyAfterAuthorityValidation(t *testing.T) {
	f, _ := newWarmContinuationFixture(t)
	client, err := pueue.NewClient(f.target, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before := parentMigrationTree(t, f.store.Root)
	if err = runUpgradeInspection(t, f, client, upgradeInspectionGuard(allowedUpgradeInspectionCommit)); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
		t.Fatal("no-head completion created an inspection group")
	}
}
