package inspection

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func groupTestStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return store
}

func groupTestBinding() task.SupervisorRef {
	return task.SupervisorRef{
		ClientExecutable:     "/usr/local/bin/pueue",
		ClientSHA256:         task.ComputeSHA256([]byte("pueue-client")),
		ResolvedConfigSHA256: task.ComputeSHA256([]byte("pueue-resolved-config")),
		Endpoint:             "unix:///tmp/pueue.sock",
		ConfigPath:           "/etc/pueue.yml",
		ConfigDigest:         task.ComputeSHA256([]byte("pueue-config")),
		ObservedVersion:      pueue.FixtureVersion,
	}
}

func groupTestTime() time.Time {
	return time.Date(2026, time.January, 1, 2, 3, 4, 5_000_000, time.UTC)
}

func openTestGroup(t *testing.T) (*taskdir.Store, *GroupJournal) {
	t.Helper()
	store := groupTestStore(t)
	group, err := OpenGroup(store, groupTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := group.Close(); err != nil {
			t.Fatal(err)
		}
	})
	return store, group
}

func testGroupSnapshot(name, status string, parallel uint64) pueue.QueueSnapshot {
	return pueue.QueueSnapshot{Groups: map[string]pueue.Group{
		name: {Status: status, ParallelTasks: parallel},
	}}
}

func TestOpenGroupBindsRootAndReplaysCanonicalRequest(t *testing.T) {
	store := groupTestStore(t)
	binding := groupTestBinding()
	first, err := OpenGroup(store, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { mustGroup(t, first.Close()) }()

	if want := groupForRoot(store.RootID); first.Name() != want {
		t.Fatalf("group name %q, want %q", first.Name(), want)
	}
	if first.Request().ParallelTasks != 1 || first.Request().RootID != store.RootID {
		t.Fatalf("unexpected immutable request: %+v", first.Request())
	}

	upgradedBinding := binding
	upgradedBinding.ClientExecutable = "/new/install/bin/pueue"
	upgradedBinding.ClientSHA256 = strings.Repeat("e", 64)
	upgradedBinding.DaemonExecutable = "/new/install/libexec/pueued"
	upgradedBinding.DaemonSHA256 = strings.Repeat("f", 64)
	upgradedBinding.ObservedVersion = "pueue 4.1.0"
	second, err := OpenGroup(store, upgradedBinding)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { mustGroup(t, second.Close()) }()
	if first.Digest() == "" || first.Digest() != second.Digest() {
		t.Fatalf("request digest changed across replay: %q / %q", first.Digest(), second.Digest())
	}
	if first.Request() != second.Request() {
		t.Fatalf("request changed across replay: %+v / %+v", first.Request(), second.Request())
	}

	var persisted GroupRequestRecord
	if err := first.control.Read(groupRequestRecordName, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != first.Request() {
		t.Fatalf("persisted request differs: %+v / %+v", persisted, first.Request())
	}
	if _, err := store.OpenTask(strings.Repeat("a", 32)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("group journal admitted ordinary task: %v", err)
	}
}

func TestOpenGroupHistoricalUsesRootForInitialGeneration(t *testing.T) {
	store := groupTestStore(t)
	binding := groupTestBinding()
	first, err := OpenGroup(store, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}

	historical, err := OpenGroupHistorical(context.Background(), store, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := historical.Close(); err != nil {
			t.Error(err)
		}
	}()
	if historical.Request().Supervisor != binding {
		t.Fatalf("historical request binding = %+v, want %+v", historical.Request().Supervisor, binding)
	}
	if _, err := os.Stat(filepath.Join(store.Root, "inspection-group-"+groupGeneration(binding))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial historical reopen created a disconnected generation: %v", err)
	}
}

func TestOpenGroupHistoricalRejectsMissingRootJournal(t *testing.T) {
	store := groupTestStore(t)
	_, err := OpenGroupHistorical(context.Background(), store, groupTestBinding())
	if !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root journal was treated as a fresh group: %v", err)
	}
}

func TestOpenGroupHistoricalReopensRetainedGenerationAfterHeadAdvances(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	firstSuccessor := legacyRequest
	firstSuccessor.Supervisor.Endpoint += ".first"
	publishTestGroupMigration(t, store, legacyRequest, firstSuccessor)
	first, err := OpenGroupGeneration(context.Background(), store, firstSuccessor.Supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	secondSuccessor := firstSuccessor
	secondSuccessor.Supervisor.Endpoint += ".second"
	publishTestGroupMigration(t, store, firstSuccessor, secondSuccessor)
	second, err := OpenGroupGeneration(context.Background(), store, secondSuccessor.Supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}

	historical, err := OpenGroupHistorical(context.Background(), store, firstSuccessor.Supervisor)
	if err != nil {
		t.Fatalf("retained generation was rejected after head advanced: %v", err)
	}
	if historical.Request().Supervisor.Endpoint != firstSuccessor.Supervisor.Endpoint {
		t.Fatalf("opened wrong retained generation: %+v", historical.Request())
	}
	if err = historical.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenGroupHistoricalMaterializesPublishedMissingGeneration(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}

	successor := legacyRequest
	successor.Supervisor.Endpoint += ".successor"
	publishTestGroupMigration(t, store, legacyRequest, successor)
	generationPath := filepath.Join(store.Root, "inspection-group-"+groupGeneration(successor.Supervisor))
	if _, err = os.Stat(generationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test did not leave successor generation absent: %v", err)
	}

	historical, err := OpenGroupHistorical(context.Background(), store, successor.Supervisor)
	if err != nil {
		t.Fatalf("published successor was not recovered: %v", err)
	}
	defer func() {
		if closeErr := historical.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if historical.Request() != successor {
		t.Fatalf("recovered successor request = %+v, want %+v", historical.Request(), successor)
	}
	if _, err = os.Stat(filepath.Join(generationPath, groupRequestRecordName)); err != nil {
		t.Fatalf("published successor generation was not materialized: %v", err)
	}
}

func TestOpenGroupHistoricalRejectsIncompleteRootJournal(t *testing.T) {
	store := groupTestStore(t)
	binding := groupTestBinding()
	group, err := OpenGroup(store, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.Root, "inspection-group", groupRequestRecordName)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGroupHistorical(context.Background(), store, binding); !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete root journal was recreated: %v", err)
	}
	successor := GroupRequestRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, Supervisor: binding, Name: groupForRoot(store.RootID), ParallelTasks: 1}
	successor.Supervisor.Endpoint += ".successor"
	legacyRequest := GroupRequestRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, Supervisor: binding, Name: groupForRoot(store.RootID), ParallelTasks: 1}
	if err := PrepareGroupMigration(context.Background(), store, legacyRequest, successor); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("damaged root allowed migration preparation: %v", err)
	}
}

func TestReadGroupHeadRejectsMissingRootWithRetainedGeneration(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	successor := legacyRequest
	successor.Supervisor.Endpoint += ".successor"
	publishTestGroupMigration(t, store, legacyRequest, successor)
	generation, err := OpenGroupGeneration(context.Background(), store, successor.Supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(store.Root, "inspection-group")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGroupHead(context.Background(), store); !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root with retained generation was treated as fresh: %v", err)
	}
	if _, err := OpenGroupHistorical(context.Background(), store, successor.Supervisor); !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical open recreated missing root: %v", err)
	}
}

func TestOpenGroupRejectsChangedBindingBeforeDirectory(t *testing.T) {
	store := groupTestStore(t)
	binding := groupTestBinding()
	group, err := OpenGroup(store, binding)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := group.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	changed := binding
	changed.ConfigDigest = task.ComputeSHA256([]byte("different-config"))
	if _, err := OpenGroup(store, changed); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed binding accepted: %v", err)
	}
}

func TestGroupHeadMatchesBindingSeparatesLogicalAndInstallationDrift(t *testing.T) {
	store := groupTestStore(t)
	binding := groupTestBinding()
	present, matches, err := GroupHeadMatchesBinding(context.Background(), store, binding)
	if err != nil || present || matches {
		t.Fatalf("missing group head reported as present=%t matches=%t err=%v", present, matches, err)
	}
	group, err := OpenGroup(store, binding)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := group.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	present, matches, err = GroupHeadMatchesBinding(context.Background(), store, binding)
	if err != nil || !present || !matches {
		t.Fatalf("matching group head reported as present=%t matches=%t err=%v", present, matches, err)
	}
	installationDrift := binding
	installationDrift.ClientExecutable = "/new/install/bin/pueue"
	installationDrift.ClientSHA256 = strings.Repeat("e", 64)
	installationDrift.DaemonExecutable = "/new/install/libexec/pueued"
	installationDrift.DaemonSHA256 = strings.Repeat("f", 64)
	present, matches, err = GroupHeadMatchesBinding(context.Background(), store, installationDrift)
	if err != nil || !present || !matches {
		t.Fatalf("installation drift changed logical match: present=%t matches=%t err=%v", present, matches, err)
	}
	configDrift := installationDrift
	configDrift.ConfigDigest = task.ComputeSHA256([]byte("new-config"))
	present, matches, err = GroupHeadMatchesBinding(context.Background(), store, configDrift)
	if err != nil || !present || matches {
		t.Fatalf("config drift was treated as logical match: present=%t matches=%t err=%v", present, matches, err)
	}
}

func TestOpenGroupWithMigrationCreatesSuccessorWithoutRewritingLegacyEvidence(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	legacyBytes, err := os.ReadFile(filepath.Join(store.Root, "inspection-group", groupRequestRecordName))
	if err != nil {
		t.Fatal(err)
	}
	closeErr := legacy.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	currentBinding := legacyBinding
	currentBinding.ConfigPath = "/private/delegation/pueue.yml"
	currentBinding.ConfigDigest = task.ComputeSHA256([]byte("private-config"))
	currentBinding.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("private-resolved-config"))
	currentBinding.Endpoint = "unix:///private/delegation/pueue.sock"
	checked := false
	group, err := OpenGroupWithMigration(context.Background(), store, currentBinding, func(_ context.Context, existing GroupRequestRecord) error {
		checked = true
		if existing != legacyRequest {
			t.Fatalf("migration checked the wrong legacy request: %+v / %+v", existing, legacyRequest)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { mustGroup(t, group.Close()) }()
	if !checked {
		t.Fatal("migration did not inspect the legacy supervisor")
	}
	if group.Name() != groupForRoot(store.RootID) || group.Request().Supervisor != currentBinding {
		t.Fatalf("successor group lost current identity: %+v", group.Request())
	}

	gotLegacy, err := os.ReadFile(filepath.Join(store.Root, "inspection-group", groupRequestRecordName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotLegacy, legacyBytes) {
		t.Fatalf("migration rewrote legacy request: before=%q after=%q", legacyBytes, gotLegacy)
	}
	variantPath := filepath.Join(store.Root, "inspection-group-"+groupGeneration(currentBinding), groupRequestRecordName)
	if _, err := os.Stat(variantPath); err != nil {
		t.Fatalf("successor request was not persisted: %v", err)
	}
}

func TestOpenGroupWithMigrationRefusesActiveLegacyWork(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	closeErr := legacy.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	currentBinding := legacyBinding
	currentBinding.ConfigDigest = task.ComputeSHA256([]byte("different-config"))
	currentBinding.Endpoint = "unix:///private/delegation/pueue.sock"
	_, err = OpenGroupWithMigration(context.Background(), store, currentBinding, func(context.Context, GroupRequestRecord) error {
		return ErrGroupMigrationBusy
	})
	if !errors.Is(err, ErrGroupMigrationBusy) {
		t.Fatalf("active legacy work was not refused: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(store.Root, "inspection-group-"+groupGeneration(currentBinding))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("migration created successor after refusal: %v", statErr)
	}
}

func TestReadGroupHeadRejectsIncompleteJournal(t *testing.T) {
	store := groupTestStore(t)
	group, err := OpenGroup(store, groupTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	if err := group.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.Root, "inspection-group", groupRequestRecordName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGroupHead(context.Background(), store); !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete inspection journal was treated as absent: %v", err)
	}
}

func TestOpenGroupWithMigrationFollowsSuccessiveGenerations(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	if closeErr := legacy.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	firstBinding := legacyBinding
	firstBinding.ConfigPath = "/private/delegation/pueue-first.yml"
	firstBinding.ConfigDigest = task.ComputeSHA256([]byte("first-config"))
	firstBinding.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("first-resolved"))
	firstBinding.Endpoint = "unix:///private/delegation/pueue-first.sock"
	firstSuccessor := legacyRequest
	firstSuccessor.Supervisor = firstBinding
	first, err := OpenGroupWithMigration(context.Background(), store, firstBinding, func(ctx context.Context, existing GroupRequestRecord) error {
		if existing != legacyRequest {
			t.Fatalf("first migration started from the wrong generation: %+v / %+v", existing, legacyRequest)
		}
		if prepareErr := PrepareGroupMigration(ctx, store, existing, firstSuccessor); prepareErr != nil {
			return prepareErr
		}
		return CompleteGroupMigration(ctx, store, existing, firstSuccessor)
	})
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := first.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	secondBinding := firstBinding
	secondBinding.ConfigPath = "/private/delegation/pueue-second.yml"
	secondBinding.ConfigDigest = task.ComputeSHA256([]byte("second-config"))
	secondBinding.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("second-resolved"))
	secondBinding.Endpoint = "unix:///private/delegation/pueue-second.sock"
	secondSuccessor := firstSuccessor
	secondSuccessor.Supervisor = secondBinding
	second, err := OpenGroupWithMigration(context.Background(), store, secondBinding, func(ctx context.Context, existing GroupRequestRecord) error {
		if existing != firstSuccessor {
			t.Fatalf("second migration did not follow the chain: %+v / %+v", existing, firstSuccessor)
		}
		if prepareErr := PrepareGroupMigration(ctx, store, existing, secondSuccessor); prepareErr != nil {
			return prepareErr
		}
		return CompleteGroupMigration(ctx, store, existing, secondSuccessor)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { mustGroup(t, second.Close()) }()
	if second.Request().Supervisor != secondBinding {
		t.Fatalf("second migration lost the newest supervisor: %+v", second.Request())
	}
	if _, err := os.Stat(filepath.Join(store.Root, "inspection-group-"+groupGeneration(secondBinding))); err != nil {
		t.Fatalf("second successor journal was not created: %v", err)
	}
}

func TestPrepareMigrationMaterializesMissingIntermediateGeneration(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	if closeErr := legacy.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	firstSuccessor := legacyRequest
	firstSuccessor.Supervisor.Endpoint = "unix:///private/delegation/first.sock"
	firstSuccessor.Supervisor.ConfigDigest = task.ComputeSHA256([]byte("first-config"))
	publishTestGroupMigration(t, store, legacyRequest, firstSuccessor)
	firstGeneration := filepath.Join(store.Root, "inspection-group-"+groupGeneration(firstSuccessor.Supervisor))
	if removeErr := os.RemoveAll(firstGeneration); removeErr != nil {
		t.Fatal(removeErr)
	}

	secondSuccessor := firstSuccessor
	secondSuccessor.Supervisor.Endpoint = "unix:///private/delegation/second.sock"
	secondSuccessor.Supervisor.ConfigDigest = task.ComputeSHA256([]byte("second-config"))
	second, err := OpenGroupWithMigration(context.Background(), store, secondSuccessor.Supervisor, func(ctx context.Context, existing GroupRequestRecord) error {
		if existing != firstSuccessor {
			t.Fatalf("migration did not recover the missing intermediate head: %+v / %+v", existing, firstSuccessor)
		}
		if migrationErr := PrepareGroupMigration(ctx, store, existing, secondSuccessor); migrationErr != nil {
			return migrationErr
		}
		return CompleteGroupMigration(ctx, store, existing, secondSuccessor)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { mustGroup(t, second.Close()) }()
	if _, err := os.Stat(filepath.Join(firstGeneration, groupRequestRecordName)); err != nil {
		t.Fatalf("missing intermediate generation was not materialized: %v", err)
	}
}

func TestGroupMigrationMarkersAreIdempotentAndBindingScoped(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	if closeErr := legacy.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	currentBinding := legacyBinding
	currentBinding.ConfigPath = "/private/delegation/pueue.yml"
	currentBinding.ConfigDigest = task.ComputeSHA256([]byte("private-config"))
	currentBinding.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("private-resolved-config"))
	currentBinding.Endpoint = "unix:///private/delegation/pueue.sock"
	successor := legacyRequest
	successor.Supervisor = currentBinding
	if prepareErr := PrepareGroupMigration(context.Background(), store, legacyRequest, successor); prepareErr != nil {
		t.Fatal(prepareErr)
	}
	requireGroupMigrationComplete(t, store, legacyRequest, successor, false)
	requireCompletedGroupMigration(t, store, false)
	if completeErr := CompleteGroupMigration(context.Background(), store, legacyRequest, successor); completeErr != nil {
		t.Fatal(completeErr)
	}
	requireGroupMigrationComplete(t, store, legacyRequest, successor, true)
	requireCompletedGroupMigration(t, store, true)
	if replayErr := PrepareGroupMigration(context.Background(), store, legacyRequest, successor); replayErr != nil {
		t.Fatalf("identical migration replay failed: %v", replayErr)
	}
	changed := successor
	changed.Supervisor.Endpoint += ".changed"
	requireGroupMigrationComplete(t, store, legacyRequest, changed, false)
}

func TestCompleteGroupMigrationRequiresExactPreparedEdge(t *testing.T) {
	store, group := openTestGroup(t)
	legacy := group.Request()
	mustGroup(t, group.Close())

	first := legacy
	first.Supervisor.Endpoint += ".first"
	second := legacy
	second.Supervisor.Endpoint += ".second"

	if err := CompleteGroupMigration(t.Context(), store, legacy, first); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unprepared edge completed: %v", err)
	}
	mustGroup(t, PrepareGroupMigration(t.Context(), store, legacy, first))
	if err := CompleteGroupMigration(t.Context(), store, legacy, second); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("different prepared edge completed: %v", err)
	}
	mustGroup(t, CompleteGroupMigration(t.Context(), store, legacy, first))
	requireGroupMigrationComplete(t, store, legacy, first, true)
	requireGroupMigrationComplete(t, store, legacy, second, false)
}

func TestGroupMigrationCompletePropagatesCorruptMarker(t *testing.T) {
	store := groupTestStore(t)
	legacy, err := OpenGroup(store, groupTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	err = legacy.Close()
	if err != nil {
		t.Fatal(err)
	}
	successor := legacyRequest
	successor.Supervisor.Endpoint += ".successor"
	publishTestGroupMigration(t, store, legacyRequest, successor)
	marker := filepath.Join(store.Root, "inspection-group", groupMigrationDoneName)
	err = os.WriteFile(marker, []byte("{"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := GroupMigrationComplete(context.Background(), store, legacyRequest, successor)
	if err == nil || complete {
		t.Fatalf("corrupt migration marker was treated as absent: complete=%t err=%v", complete, err)
	}
}

func TestPrepareGroupMigrationKeepsSuccessivePendingSuccessorsReversible(t *testing.T) {
	store := groupTestStore(t)
	legacyBinding := groupTestBinding()
	legacy, err := OpenGroup(store, legacyBinding)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := legacy.Request()
	if closeErr := legacy.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	first := legacyRequest
	first.Supervisor.Endpoint = "unix:///private/delegation/first.sock"
	first.Supervisor.ConfigDigest = task.ComputeSHA256([]byte("first-config"))
	second := first
	second.Supervisor.Endpoint = "unix:///private/delegation/second.sock"
	second.Supervisor.ConfigDigest = task.ComputeSHA256([]byte("second-config"))
	if prepareErr := PrepareGroupMigration(context.Background(), store, legacyRequest, first); prepareErr != nil {
		t.Fatal(prepareErr)
	}
	if prepareErr := PrepareGroupMigration(context.Background(), store, legacyRequest, second); prepareErr != nil {
		t.Fatalf("changed pending successor wedged migration: %v", prepareErr)
	}
	if _, statErr := os.Stat(filepath.Join(store.Root, "inspection-group", pendingMigrationRecordName(groupRequestDigest(second)))); statErr != nil {
		t.Fatalf("successive pending marker was not retained: %v", statErr)
	}
	if completeErr := CompleteGroupMigration(context.Background(), store, legacyRequest, second); completeErr != nil {
		t.Fatal(completeErr)
	}
	complete, err := GroupMigrationComplete(context.Background(), store, legacyRequest, second)
	if err != nil || !complete {
		t.Fatalf("successive pending marker did not complete: %t %v", complete, err)
	}
}

func requireGroupMigrationComplete(t *testing.T, store *taskdir.Store, legacy, successor GroupRequestRecord, want bool) {
	t.Helper()
	complete, err := GroupMigrationComplete(context.Background(), store, legacy, successor)
	if err != nil || complete != want {
		t.Fatalf("migration completion=%t, want %t: %v", complete, want, err)
	}
}

func requireCompletedGroupMigration(t *testing.T, store *taskdir.Store, want bool) {
	t.Helper()
	complete, err := HasCompletedGroupMigration(context.Background(), store)
	if err != nil || complete != want {
		t.Fatalf("completed lifecycle marker=%t, want %t: %v", complete, want, err)
	}
}

func TestGroupCreateClaimIsSingleUseAndConcurrentReplayNeverGrants(t *testing.T) {
	_, group := openTestGroup(t)
	now := groupTestTime()
	permits := make([]*GroupPermit, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range permits {
		index := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			permits[index], errs[index] = group.ClaimCreate(now)
		}()
	}
	wg.Wait()

	winners := 0
	for i := range permits {
		if permits[i] != nil {
			winners++
			if errs[i] != nil {
				t.Fatalf("winner returned error: %v", errs[i])
			}
			if permits[i].InspectionGroupRootID() != group.Request().RootID || permits[i].InspectionGroupSupervisor() != group.Request().Supervisor {
				t.Fatalf("group permit lost immutable request binding: root=%q supervisor=%+v request=%+v", permits[i].InspectionGroupRootID(), permits[i].InspectionGroupSupervisor(), group.Request())
			}
			if err := permits[i].Consume(); err != nil {
				t.Fatal(err)
			}
			if err := permits[i].Consume(); !errors.Is(err, task.ErrPermitAlreadyUsed) {
				t.Fatalf("permit reused: %v", err)
			}
			continue
		}
		if !errors.Is(errs[i], task.ErrAlreadySubmitted) {
			t.Fatalf("losing claim error %v", errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("got %d create permits, want one", winners)
	}
}

func TestGroupCreateReplayRetainsOriginalGuardAtDifferentTime(t *testing.T) {
	store, group := openTestGroup(t)
	first, err := group.ClaimCreate(groupTestTime())
	if err != nil || first == nil {
		t.Fatalf("initial group claim: permit=%v err=%v", first, err)
	}
	before, err := os.ReadFile(filepath.Join(store.Root, "inspection-group", groupCreationRecordName))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := group.ClaimCreate(groupTestTime().Add(time.Second))
	if replay != nil || !errors.Is(err, task.ErrAlreadySubmitted) {
		t.Fatalf("group replay at a different time: permit=%v err=%v", replay, err)
	}
	after, err := os.ReadFile(filepath.Join(store.Root, "inspection-group", groupCreationRecordName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("group replay changed guard bytes: before=%q after=%q", before, after)
	}
}

func TestGroupCreateReplayRejectsMalformedOrWrongDigestGuard(t *testing.T) {
	tests := []struct {
		name   string
		raw    []byte
		record *GroupCreationRecord
	}{
		{name: "malformed", raw: []byte("not-json")},
		{name: "wrong digest", record: &GroupCreationRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: strings.Repeat("b", 64), CreatedAt: groupTestTime().UTC().Format(time.RFC3339Nano)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, group := openTestGroup(t)
			path := filepath.Join(store.Root, "inspection-group", groupCreationRecordName)
			if test.raw != nil {
				if err := os.WriteFile(path, test.raw, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if _, err := group.control.Put(groupCreationRecordName, test.record); err != nil {
				t.Fatal(err)
			}
			permit, err := group.ClaimCreate(groupTestTime().Add(time.Second))
			if permit != nil || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("invalid existing group guard accepted: permit=%v err=%v", permit, err)
			}
		})
	}
}

func TestGroupCreateUncertainPublicationNeverGrantsReplay(t *testing.T) {
	store, group := openTestGroup(t)
	store.SetFaultInjector(&groupPostLinkFault{target: groupCreationRecordName})
	permit, err := group.ClaimCreate(groupTestTime())
	if permit != nil || !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("uncertain create granted authority: permit=%v err=%v", permit, err)
	}
	store.SetFaultInjector(nil)
	permit, err = group.ClaimCreate(groupTestTime())
	if permit != nil || !errors.Is(err, task.ErrAlreadySubmitted) {
		t.Fatalf("uncertain replay granted authority: permit=%v err=%v", permit, err)
	}
}

func TestGroupCreateRejectsInvalidTimeWithoutWriting(t *testing.T) {
	_, group := openTestGroup(t)
	if permit, err := group.ClaimCreate(time.Time{}); permit != nil || !errors.Is(err, ErrInvalidAdmissionTime) {
		t.Fatalf("invalid time accepted: permit=%v err=%v", permit, err)
	}
	var record GroupCreationRecord
	if err := group.control.Read(groupCreationRecordName, &record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid time wrote creation guard: %v", err)
	}
}

func TestGroupCreateContextCancellationDoesNotPublishGuard(t *testing.T) {
	_, group := openTestGroup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if permit, err := group.ClaimCreateContext(ctx, groupTestTime()); permit != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled group claim yielded authority: permit=%v err=%v", permit, err)
	}
	var record GroupCreationRecord
	if err := group.control.Read(groupCreationRecordName, &record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled group claim wrote creation guard: %v", err)
	}
}

func TestGroupObserveRequiresFreshRunningSingleSlotSnapshot(t *testing.T) {
	_, group := openTestGroup(t)
	name := group.Name()
	for _, test := range []struct {
		name     string
		snapshot pueue.QueueSnapshot
	}{
		{name: "missing", snapshot: pueue.QueueSnapshot{}},
		{name: "paused", snapshot: testGroupSnapshot(name, "Paused", 1)},
		{name: "reset", snapshot: testGroupSnapshot(name, "Reset", 1)},
		{name: "parallel-zero", snapshot: testGroupSnapshot(name, "Running", 0)},
		{name: "parallel-two", snapshot: testGroupSnapshot(name, "Running", 2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := group.Observe(test.snapshot); !errors.Is(err, pueue.ErrUnknown) {
				t.Fatalf("invalid snapshot error %v", err)
			}
		})
	}
	var absent GroupObservationRecord
	if err := group.control.Read(groupObservationRecordName, &absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid snapshot persisted observation: %v", err)
	}

	valid := testGroupSnapshot(name, "Running", 1)
	if err := group.Observe(valid); err != nil {
		t.Fatal(err)
	}
	var persisted GroupObservationRecord
	if err := group.control.Read(groupObservationRecordName, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.RequestSHA256 != group.Digest() || persisted.Name != name || persisted.Status != "Running" || persisted.ParallelTasks != 1 {
		t.Fatalf("bad observation record: %+v", persisted)
	}
	if err := group.Observe(valid); err != nil {
		t.Fatalf("identical fresh replay failed: %v", err)
	}
	if err := group.Observe(testGroupSnapshot(name, "Paused", 1)); !errors.Is(err, pueue.ErrUnknown) {
		t.Fatalf("stale observation accepted as current: %v", err)
	}
}

func TestGroupObserveRequiresExactDerivedNameAndClosedHandleHasNoAuthority(t *testing.T) {
	_, group := openTestGroup(t)
	wrong := testGroupSnapshot(group.Name()+"-other", "Running", 1)
	if err := group.Observe(wrong); !errors.Is(err, pueue.ErrUnknown) {
		t.Fatalf("wrong group was accepted: %v", err)
	}
	if err := group.Close(); err != nil {
		t.Fatal(err)
	}
	if permit, err := group.ClaimCreate(groupTestTime()); permit != nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed group granted create authority: permit=%v err=%v", permit, err)
	}
	if err := group.Observe(testGroupSnapshot(group.Name(), "Running", 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed group accepted observation: %v", err)
	}
}

func mustGroup(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func publishTestGroupMigration(t *testing.T, store *taskdir.Store, legacy, successor GroupRequestRecord) {
	t.Helper()
	mustGroup(t, PrepareGroupMigration(t.Context(), store, legacy, successor))
	mustGroup(t, CompleteGroupMigration(t.Context(), store, legacy, successor))
}

type groupPostLinkFault struct {
	taskdir.BaseFaultInjector
	target string
}

func (f *groupPostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == f.target {
		return errors.New("injected group post-link barrier fault")
	}
	return nil
}
