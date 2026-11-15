package supervisor

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The settled generation and finite control scripts are DATA/component inputs;
// they grant no native birth or process closure. Native restart has its own app
// consumer regression with an actual daemon and its sole Wait owner.
func unpreparedRecoveryFixture(t *testing.T) (parentMigrationFixture, infrastructure.SavedBindingSource, *pueue.Client) {
	t.Helper()
	f, source := newWarmContinuationFixture(t)
	writeGenerationGroup(t, f.store, f.legacy)
	client, err := pueue.NewClient(f.target, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return f, source, client
}

func unpreparedMigrationFixture(t *testing.T) (parentMigrationFixture, *pueue.Client) {
	t.Helper()
	f := newSavedControlFixture(t)
	f.legacy = f.target
	f.legacy.ConfigDigest = task.ComputeSHA256([]byte("closed historical migration configuration"))
	f.legacy.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("closed historical resolved configuration"))
	base := filepath.Dir(f.target.ConfigPath)
	for _, name := range []string{"data", "run"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "bootstrap.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeContinuationGeneration(t, f, []parentMigrationRecordFixture{{task.SchemaVersion, parentMigrationIdentity(f.legacy), parentMigrationIdentity(f.target), 12345, true}})
	writeGenerationGroup(t, f.store, f.legacy)
	client, err := pueue.NewClient(f.target, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return f, client
}

func recoveryGeneration(t *testing.T, ctx context.Context, f parentMigrationFixture) Generation {
	t.Helper()
	resolution, err := ResolveGenerationWithLock(ctx, f.store, f.target)
	if err != nil || !resolution.GroupCleanupDue || resolution.Active != f.target {
		t.Fatalf("stale completed generation was not retained: resolution=%+v err=%v", resolution, err)
	}
	return resolution
}

func TestUnpreparedInspectionRecoveryKeepsAdmissionAndContinuationBlocked(t *testing.T) {
	f, _, client := unpreparedRecoveryFixture(t)
	for _, consumer := range []string{"shared recovery", "inspection admission", "continuation successor"} {
		t.Run(consumer, func(t *testing.T) {
			before := parentMigrationTree(t, f.store.Root)
			var err error
			if consumer == "inspection admission" {
				err = EnsureInspectionGroupWithMigrationContext(t.Context(), f.store, client, time.Second, false)
			} else {
				err = pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
					if consumer == "shared recovery" {
						return CompletePendingGroupMigrationWithLock(ctx, f.store, recoveryGeneration(t, ctx, f))
					}
					ready, validateErr := RevalidateContinuationSuccessor(ctx, f.store.Root, f.target, openRecoveryMetadataStore)
					if ready != (task.SupervisorRef{}) {
						t.Fatal("unprepared continuation returned a ready successor")
					}
					return validateErr
				})
			}
			if !errors.Is(err, inspection.ErrGroupMigrationRequired) || errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("unprepared edge lost honest migrate refusal: %v", err)
			}
			if !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
				t.Fatal("shared consumer manufactured inspection preparation or completion")
			}
		})
	}
}

func openRecoveryMetadataStore(ctx context.Context, root string) (*taskdir.Store, error) {
	return taskdir.OpenExistingStoreMetadataContext(ctx, root, predicate.Default())
}

func TestUnpreparedInspectionContinuationCannotWarmReuse(t *testing.T) {
	f, source, _ := unpreparedRecoveryFixture(t)
	var routed, commands atomic.Int32
	f.options.Observer = func(pueue.CommandEvent) { commands.Add(1) }
	routing := selectedControlRouting{continuation: func(ctx context.Context, store *taskdir.Store, _ infrastructure.LaunchContext, actual infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		routed.Add(1)
		if actual != source {
			t.Fatal("continuation replaced its immutable saved source")
		}
		var resultErr error
		err := pueue.WithBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
			_, resultErr = RevalidateContinuationSuccessor(locked, store.Root, f.target, openRecoveryMetadataStore)
			return resultErr
		})
		return infrastructure.Disposition{}, err
	}}
	before := parentMigrationTree(t, f.store.Root)
	client, err := (ControlRuntime{Options: f.options, Routing: routing}).PrepareContinuation(t.Context(), f.store, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("unprepared continuation returned warm client")))
	}
	if !errors.Is(err, inspection.ErrGroupMigrationRequired) || errors.Is(err, task.ErrEvidenceFault) || routed.Load() != 1 || commands.Load() != 0 {
		t.Fatalf("unprepared continuation bypassed repair: routes=%d commands=%d err=%v", routed.Load(), commands.Load(), err)
	}
	if !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
		t.Fatal("unprepared continuation changed admission or provider authority")
	}
}

func TestUnpreparedInspectionAlternateUsesExistingPreparationPath(t *testing.T) {
	for _, coordinator := range []bool{false, true} {
		name := "existing migration callback"
		if coordinator {
			name = "explicit migration coordinator"
		}
		t.Run(name, func(t *testing.T) {
			f, client := unpreparedMigrationFixture(t)
			legacy := InspectionGroup(f.store.RootID, f.legacy)
			alternate := InspectionGroup(f.store.RootID, f.target)
			alternate.Supervisor.ConfigDigest = task.ComputeSHA256([]byte("unaccepted alternate proposal"))
			if err := inspection.PrepareGroupMigration(t.Context(), f.store, legacy, alternate); err != nil {
				t.Fatal(err)
			}
			err := pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
				if coordinator {
					return MigrateInspectionSupervisorLocked(ctx, f.store, client, time.Second)
				}
				return MigrateInspectionGroup(ctx, f.store, client, time.Second, legacy)
			})
			if err != nil {
				t.Fatalf("valid alternate could not use existing fresh prepare: %v", err)
			}
			complete, err := inspection.GroupMigrationComplete(t.Context(), f.store, legacy, InspectionGroup(f.store.RootID, f.target))
			if err != nil || !complete {
				t.Fatalf("canonical selected alternate was not completed: complete=%t err=%v", complete, err)
			}
		})
	}
}

func TestUnpreparedInspectionRecoveryRetainsCanonicalJournalRules(t *testing.T) {
	for _, mode := range []string{"exact prepared", "exact completed", "exact alternate", "corrupt prepared", "corrupt unrelated alternate", "completed fork", "changed full head", "disconnected private lineage"} {
		t.Run(mode, func(t *testing.T) { testUnpreparedRecoveryJournalRule(t, mode) })
	}
}

func testUnpreparedRecoveryJournalRule(t *testing.T, mode string) {
	t.Helper()
	f, _, _ := unpreparedRecoveryFixture(t)
	legacy, target := InspectionGroup(f.store.RootID, f.legacy), InspectionGroup(f.store.RootID, f.target)
	err := pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		resolution := configureRecoveryJournalRule(t, ctx, f, mode)
		before := parentMigrationTree(t, f.store.Root)
		resultErr := CompletePendingGroupMigrationWithLock(ctx, f.store, resolution)
		if invalidRecoveryJournalRule(mode) && !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
			t.Fatal("invalid journal was rewritten")
		}
		return resultErr
	})
	if invalidRecoveryJournalRule(mode) {
		want := task.ErrEvidenceFault
		if mode == "disconnected private lineage" {
			want = inspection.ErrGroupMigrationRequired
		}
		if !errors.Is(err, want) {
			t.Fatalf("invalid edge lost its canonical fault: want=%v err=%v", want, err)
		}
		if _, unprepared := err.(interface{ unpreparedInspectionMigration() }); unprepared {
			t.Fatalf("invalid edge acquired fresh-prepare classification: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), f.store, legacy, target)
	if err != nil || !complete {
		t.Fatalf("canonical prepared/completed edge did not recover: complete=%t err=%v", complete, err)
	}
}

func invalidRecoveryJournalRule(mode string) bool {
	return mode == "corrupt prepared" || mode == "corrupt unrelated alternate" || mode == "completed fork" || mode == "changed full head" || mode == "disconnected private lineage"
}

func configureRecoveryJournalRule(t *testing.T, ctx context.Context, f parentMigrationFixture, mode string) Generation {
	t.Helper()
	resolution := recoveryGeneration(t, ctx, f)
	legacy, target := InspectionGroup(f.store.RootID, f.legacy), InspectionGroup(f.store.RootID, f.target)
	other := target
	other.Supervisor.ConfigDigest = task.ComputeSHA256([]byte("different journal target"))
	prepare := func(successor inspection.GroupRequestRecord) {
		if err := inspection.PrepareGroupMigration(ctx, f.store, legacy, successor); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(successor inspection.GroupRequestRecord) {
		if err := inspection.CompleteGroupMigration(ctx, f.store, legacy, successor); err != nil {
			t.Fatal(err)
		}
	}
	switch mode {
	case "exact prepared":
		prepare(target)
	case "exact completed":
		prepare(target)
		complete(target)
	case "exact alternate":
		prepare(other)
		prepare(target)
	case "corrupt prepared":
		prepare(target)
		if err := os.WriteFile(filepath.Join(f.store.Root, "inspection-group", "migration.json"), []byte("not-json\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "corrupt unrelated alternate":
		prepare(target)
		prepare(other)
		corruptRecoveryAlternate(t, f.store.Root)
	case "completed fork":
		prepare(other)
		complete(other)
	case "changed full head":
		changed := legacy
		changed.Supervisor.ClientExecutable = target.Supervisor.ClientExecutable + "-other"
		writeContinuationFixtureRecord(t, filepath.Join(f.store.Root, "inspection-group", "request.json"), changed)
	case "disconnected private lineage":
		resolution.Active = other.Supervisor
	default:
		t.Fatal("unknown journal rule")
	}
	return resolution
}

func TestUnpreparedInspectionHistoricalProposalUsesCurrentCompletedLineage(t *testing.T) {
	for _, coordinator := range []bool{false, true} {
		name := "existing callback"
		if coordinator {
			name = "explicit coordinator"
		}
		t.Run(name, func(t *testing.T) { testHistoricalInspectionProposal(t, coordinator) })
	}
}

func testHistoricalInspectionProposal(t *testing.T, coordinator bool) {
	t.Helper()
	f, client := unpreparedMigrationFixture(t)
	middle := f.target
	middle.ConfigDigest = task.ComputeSHA256([]byte("completed intermediate private generation"))
	writeContinuationGeneration(t, f, []parentMigrationRecordFixture{
		{task.SchemaVersion, parentMigrationIdentity(f.legacy), parentMigrationIdentity(middle), 12345, true},
		{task.SchemaVersion, parentMigrationIdentity(middle), parentMigrationIdentity(f.target), 12346, true},
	})
	legacy := InspectionGroup(f.store.RootID, f.legacy)
	if err := inspection.PrepareGroupMigration(t.Context(), f.store, legacy, InspectionGroup(f.store.RootID, middle)); err != nil {
		t.Fatal(err)
	}
	err := pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		if coordinator {
			return MigrateInspectionSupervisorLocked(ctx, f.store, client, time.Second)
		}
		return MigrateInspectionGroup(ctx, f.store, client, time.Second, legacy)
	})
	if err != nil {
		t.Fatalf("historical accepted proposal wedged current canonical target: %v", err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), f.store, legacy, InspectionGroup(f.store.RootID, f.target))
	if err != nil || !complete {
		t.Fatalf("historical proposal selected an obsolete private target: complete=%t err=%v", complete, err)
	}
}

type inspectionRecoveryCommitFault struct {
	taskdir.BaseFaultInjector
	cause error
	hits  int
}

func (f *inspectionRecoveryCommitFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == "migration-complete.json" {
		f.hits++
		return f.cause
	}
	return nil
}

func TestUnpreparedInspectionPreparedRecoveryRetainsInterruptedCommit(t *testing.T) {
	f, _, _ := unpreparedRecoveryFixture(t)
	legacy, target := InspectionGroup(f.store.RootID, f.legacy), InspectionGroup(f.store.RootID, f.target)
	if err := inspection.PrepareGroupMigration(t.Context(), f.store, legacy, target); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("inspection completion directory barrier interrupted")
	fault := &inspectionRecoveryCommitFault{cause: failure}
	t.Cleanup(func() { f.store.SetFaultInjector(nil) })
	err := pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		resolution := recoveryGeneration(t, ctx, f)
		f.store.SetFaultInjector(fault)
		commitErr := CompletePendingGroupMigrationWithLock(ctx, f.store, resolution)
		f.store.SetFaultInjector(nil)
		if !errors.Is(commitErr, failure) || fault.hits != 1 {
			t.Fatalf("prepared completion hid its persistence error: hits=%d err=%v", fault.hits, commitErr)
		}
		if _, unprepared := commitErr.(interface{ unpreparedInspectionMigration() }); unprepared {
			t.Fatal("interrupted prepared write became fresh-prepare authority")
		}
		before := parentMigrationTree(t, f.store.Root)
		replayErr := CompletePendingGroupMigrationWithLock(ctx, f.store, resolution)
		if !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
			t.Fatal("completed-edge recovery rewrote the canonical winner")
		}
		return replayErr
	})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), f.store, legacy, target)
	if err != nil || !complete {
		t.Fatalf("interrupted completion did not retain the exact winner: complete=%t err=%v", complete, err)
	}
}

func TestUnpreparedInspectionSameLogicalOtherPairRequiresSelectedPreparation(t *testing.T) {
	f, client := unpreparedMigrationFixture(t)
	other := parentMigrationPair(t, f, "other-installed-pair")
	if !task.SameSupervisorIdentity(other, f.target) || other == f.target {
		t.Fatal("fixture does not distinguish full pair from logical identity")
	}
	legacy := InspectionGroup(f.store.RootID, f.legacy)
	if err := inspection.PrepareGroupMigration(t.Context(), f.store, legacy, InspectionGroup(f.store.RootID, other)); err != nil {
		t.Fatal(err)
	}
	err := pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		before := parentMigrationTree(t, f.store.Root)
		sharedErr := CompletePendingGroupMigrationWithLock(ctx, f.store, recoveryGeneration(t, ctx, f))
		if !errors.Is(sharedErr, inspection.ErrGroupMigrationRequired) || errors.Is(sharedErr, task.ErrEvidenceFault) || !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
			t.Fatalf("other full pair acquired shared completion authority: %v", sharedErr)
		}
		return MigrateInspectionSupervisorLocked(ctx, f.store, client, time.Second)
	})
	if err != nil {
		t.Fatalf("explicit selected target could not prepare its exact full pair: %v", err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), f.store, legacy, InspectionGroup(f.store.RootID, f.target))
	if err != nil || !complete {
		t.Fatalf("selected full target was not completed: complete=%t err=%v", complete, err)
	}
}

func TestUnpreparedInspectionAlreadyMigratedSourceCanAdvanceAgain(t *testing.T) {
	f, client := unpreparedMigrationFixture(t)
	middle := f.target
	middle.ConfigDigest = task.ComputeSHA256([]byte("earlier completed inspection generation"))
	writeContinuationGeneration(t, f, []parentMigrationRecordFixture{
		{task.SchemaVersion, parentMigrationIdentity(f.legacy), parentMigrationIdentity(middle), 12345, true},
		{task.SchemaVersion, parentMigrationIdentity(middle), parentMigrationIdentity(f.target), 12346, true},
	})
	legacy, source := InspectionGroup(f.store.RootID, f.legacy), InspectionGroup(f.store.RootID, middle)
	if err := inspection.PrepareGroupMigration(t.Context(), f.store, legacy, source); err != nil {
		t.Fatal(err)
	}
	if err := inspection.CompleteGroupMigration(t.Context(), f.store, legacy, source); err != nil {
		t.Fatal(err)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), f.store)
	if err != nil || !migrated || head != source {
		t.Fatalf("fixture has no prior completed inspection chain: migrated=%t err=%v", migrated, err)
	}
	err = pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
		return MigrateInspectionSupervisorLocked(ctx, f.store, client, time.Second)
	})
	if err != nil {
		t.Fatalf("unchanged source was mistaken for a new completed fork: %v", err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), f.store, source, InspectionGroup(f.store.RootID, f.target))
	if err != nil || !complete {
		t.Fatalf("later exact inspection edge was not completed: complete=%t err=%v", complete, err)
	}
}

func TestUnpreparedInspectionFreshPrepareRequiresActualRootQuiescence(t *testing.T) {
	for _, blocker := range []string{"active foreign queue", "ended unproven inspection", "malformed root resource"} {
		t.Run(blocker, func(t *testing.T) {
			f, client := unpreparedMigrationFixture(t)
			configureFreshInspectionBlocker(t, f, blocker)
			before := parentMigrationTree(t, f.store.Root)
			err := pueue.WithBootstrapLockContext(t.Context(), f.store.Root, func(ctx context.Context) error {
				return MigrateInspectionSupervisorLocked(ctx, f.store, client, time.Second)
			})
			if err == nil || !maps.Equal(before, parentMigrationTree(t, f.store.Root)) {
				t.Fatalf("nonquiescent root acquired inspection/private migration records: %v", err)
			}
			assertFreshInspectionBlocker(t, blocker, err)
		})
	}
}

func configureFreshInspectionBlocker(t *testing.T, f parentMigrationFixture, blocker string) {
	t.Helper()
	switch blocker {
	case "active foreign queue":
		stamp := "2026-09-13T20:00:00.123456789Z"
		job := map[string]any{"id": 7, "created_at": stamp, "original_command": "foreign fixture", "command": "foreign fixture", "path": "/tmp", "envs": map[string]string{}, "group": "default", "dependencies": []int{}, "priority": 0, "label": nil, "status": map[string]any{"Running": map[string]any{"enqueued_at": stamp, "start": stamp}}}
		writeContinuationFixtureRecord(t, f.status, map[string]any{"tasks": map[string]any{"7": job}, "groups": map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}}})
	case "ended unproven inspection":
		data := custodyCleanupFixture{store: f.store, supervisor: f.target, legacy: InspectionGroup(f.store.RootID, f.legacy)}
		data.unpreparedInspection(t, "b", 7, inspection.Binding{})
	case "malformed root resource":
		if err := os.MkdirAll(filepath.Join(f.store.Root, "workspace-operations", strings.Repeat("c", 32)), 0o700); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown quiescence blocker")
	}
}

func assertFreshInspectionBlocker(t *testing.T, blocker string, cause error) {
	t.Helper()
	switch blocker {
	case "active foreign queue":
		if !errors.Is(cause, ErrRuntimeQuiescenceBusy) {
			t.Fatalf("actual active queue lost its blocker: %v", cause)
		}
	case "ended unproven inspection":
		var resource *RuntimeMaintenanceError
		if !errors.As(cause, &resource) || resource.Kind != "inspection" {
			t.Fatalf("ended unproven inspection was not independently observed: %v", cause)
		}
	case "malformed root resource":
		var resource *RuntimeMaintenanceError
		if MaintenanceBusy(cause) || !errors.As(cause, &resource) || resource.Kind != "workspace_operation" || resource.ID != strings.Repeat("c", 32) || !errors.Is(cause, os.ErrNotExist) {
			t.Fatalf("malformed root resource lost its typed missing-evidence fault: %v", cause)
		}
	default:
		t.Fatal("unknown quiescence blocker")
	}
}

func corruptRecoveryAlternate(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "inspection-group"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "migration-") {
			if err := os.WriteFile(filepath.Join(root, "inspection-group", entry.Name()), []byte("not-json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
