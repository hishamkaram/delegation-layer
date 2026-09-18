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

	second, err := OpenGroup(store, binding)
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

func TestOpenGroupRejectsChangedBindingBeforeDirectory(t *testing.T) {
	store := groupTestStore(t)
	binding := groupTestBinding()
	group, err := OpenGroup(store, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.Close(); err != nil {
		t.Fatal(err)
	}
	changed := binding
	changed.ConfigDigest = task.ComputeSHA256([]byte("different-config"))
	if _, err := OpenGroup(store, changed); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed binding accepted: %v", err)
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
