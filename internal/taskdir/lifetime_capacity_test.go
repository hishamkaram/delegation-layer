package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	capacityTestStartedAt = "2026-01-02T03:04:05Z"
	formerLaunchLimit     = 64

	// scenarioCap bounds a scenario that issues many durable operations; it
	// is far above the observed loaded runtime and below the default
	// `go test` timeout. scenarioCleanupReserve is left for store and
	// directory cleanup before the test binary's own deadline.
	scenarioCap            = 10 * time.Minute
	scenarioCleanupReserve = 10 * time.Second
)

// scenarioDeadline is the one finite deadline of a multi-operation scenario:
// min(now+scenarioCap, t.Deadline()-scenarioCleanupReserve).
func scenarioDeadline(t *testing.T) time.Time {
	t.Helper()
	deadline := time.Now().Add(scenarioCap)
	if limit, ok := t.Deadline(); ok {
		if reserved := limit.Add(-scenarioCleanupReserve); reserved.Before(deadline) {
			deadline = reserved
		}
	}
	return deadline
}

// scenarioContext carries scenarioDeadline into every operation of a scenario
// so no call falls back to the short per-operation persistence timeout.
func scenarioContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(t.Context(), scenarioDeadline(t))
	t.Cleanup(cancel)
	return ctx
}

func registeredCapacityTask(t testing.TB) (*TaskDir, string) {
	t.Helper()
	store, err := InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})
	req, meta, brief := preparedInput(t, store)
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := td.AcquireLifetimeOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Errorf("release owner: %v", releaseErr)
		}
	})
	deadline := time.Now().Add(time.Hour).UTC()
	if _, err = td.RecordLifetimeIntent(LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-capacity", BootID: "boot-token", Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = td.RecordLifetimeRegistration(LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopeID: "process-group:capacity", BootID: "boot-token", Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	digest, err := td.LifetimeRegistrationDigest()
	if err != nil {
		t.Fatal(err)
	}
	return td, digest
}

func capacityLaunchRecord(td *TaskDir, registrationDigest string, sequence uint64, processStart string) task.LifetimeLaunchRecord {
	return task.LifetimeLaunchRecord{
		SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID,
		RegistrationSHA256: registrationDigest, Sequence: sequence, PID: 1000 + int(sequence),
		ProcessStart: processStart, PGID: 1000 + int(sequence), StartedAt: capacityTestStartedAt,
	}
}

// seedTaskLaunchFile writes one valid canonical launch record without the
// durable writer, so boundary tests need not run thousands of barriers.
func seedTaskLaunchFile(t testing.TB, td *TaskDir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(td.Dir, lifetimeDirectory, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedTaskLaunch(t testing.TB, td *TaskDir, registrationDigest string, sequence uint64, processStart string) {
	t.Helper()
	data, err := task.MarshalCanonical(capacityLaunchRecord(td, registrationDigest, sequence, processStart))
	if err != nil {
		t.Fatal(err)
	}
	seedTaskLaunchFile(t, td, lifetimeLaunchName(sequence), data)
}

// processStartForCanonicalSize pads a launch record to an exact canonical size.
func processStartForCanonicalSize(t testing.TB, td *TaskDir, registrationDigest string, sequence uint64, size int) string {
	t.Helper()
	base, err := task.MarshalCanonical(capacityLaunchRecord(td, registrationDigest, sequence, "x"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Repeat("x", size-len(base)+1)
}

// The ordinary store records, reads and terminates more launches than the
// former 64-launch namespace through its durable writer.
func TestTaskLifetimeLaunchesPersistBeyondFormerLimit(t *testing.T) {
	td, _ := registeredCapacityTask(t)
	count := uint64(formerLaunchLimit + 6)
	for sequence := uint64(1); sequence <= count; sequence++ {
		if _, err := td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: sequence, PID: 1000 + int(sequence), ProcessStart: "native:start", PGID: 1000 + int(sequence)}); err != nil {
			t.Fatalf("record launch %d: %v", sequence, err)
		}
	}
	launches, err := td.ReadLifetimeLaunches()
	must(t, err)
	termination, err := td.RecordLifetimeTermination(task.LifetimeTerminationRecovery)
	must(t, err)
	digest, err := task.LifetimeLaunchSetDigest(launches)
	must(t, err)
	if uint64(len(launches)) != count || termination.LaunchCount != count || termination.LaunchSetSHA256 != digest {
		t.Fatalf("launches = %d, termination = %d %s, want %d %s", len(launches), termination.LaunchCount, termination.LaunchSetSHA256, count, digest)
	}
}

// The full namespace is readable; any name beyond it is refused by the
// inventory before a record is read, so an unreadable record cannot mask it.
func TestTaskLifetimeLaunchInventoryAdmitsFullNamespaceOnly(t *testing.T) {
	td, digest := registeredCapacityTask(t)
	for sequence := uint64(1); sequence <= task.MaxLifetimeLaunches; sequence++ {
		seedTaskLaunch(t, td, digest, sequence, "native:start")
	}
	launches, err := td.ReadLifetimeLaunches()
	must(t, err)
	if uint64(len(launches)) != task.MaxLifetimeLaunches {
		t.Fatalf("launches = %d, want %d", len(launches), task.MaxLifetimeLaunches)
	}
	seedTaskLaunchFile(t, td, lifetimeLaunchName(1), []byte("not a record"))
	seedTaskLaunchFile(t, td, lifetimeLaunchName(task.MaxLifetimeLaunches+1), nil)
	if _, err = td.ReadLifetimeLaunches(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("read beyond namespace = %v, want evidence fault before any record read", err)
	}
}

// The aggregate byte limit holds independently of the count: 64 maximal
// records remain readable, and a 65th is refused before publication.
func TestTaskLifetimeLaunchesEnforceAggregateBytesBeforePublication(t *testing.T) {
	td, digest := registeredCapacityTask(t)
	for sequence := uint64(1); sequence <= formerLaunchLimit; sequence++ {
		seedTaskLaunch(t, td, digest, sequence, processStartForCanonicalSize(t, td, digest, sequence, task.MaxControlRecordSize))
	}
	launches, err := td.ReadLifetimeLaunches()
	must(t, err)
	if len(launches) != formerLaunchLimit {
		t.Fatalf("launches = %d, want %d maximal records", len(launches), formerLaunchLimit)
	}
	// The existing winner is already counted: rerecording it at the exact
	// byte limit succeeds instead of counting it twice.
	last := launches[formerLaunchLimit-1]
	if _, err = td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: last.Sequence, PID: last.PID, ProcessStart: last.ProcessStart, PGID: last.PGID}); err != nil {
		t.Fatalf("idempotent rerecord at the byte limit: %v", err)
	}
	next := uint64(formerLaunchLimit + 1)
	_, err = td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: next, PID: 2000, ProcessStart: strings.Repeat("x", 1024), PGID: 2000})
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("write beyond aggregate bytes = %v, want evidence fault", err)
	}
	if _, statErr := os.Stat(filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(next))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused launch record was published: %v", statErr)
	}
	seedTaskLaunch(t, td, digest, next, "native:start")
	if _, err = td.ReadLifetimeLaunches(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("read beyond aggregate bytes = %v, want evidence fault", err)
	}
}

func TestTaskLifetimeLaunchInventoryRefusesGapsAndMalformedNames(t *testing.T) {
	tests := []struct {
		name      string
		sequences []uint64
		extra     string
		want      error
	}{
		{name: "gap before later record", sequences: []uint64{1, 3}, want: task.ErrIdentityMismatch},
		{name: "missing first", sequences: []uint64{2}, want: task.ErrIdentityMismatch},
		{name: "malformed sequence", sequences: []uint64{1}, extra: "0000000000000000000x" + lifetimeLaunchSuffix, want: task.ErrIdentityMismatch},
		{name: "short sequence", sequences: []uint64{1}, extra: "2" + lifetimeLaunchSuffix, want: task.ErrIdentityMismatch},
		{name: "overflow", sequences: []uint64{1}, extra: "99999999999999999999" + lifetimeLaunchSuffix, want: task.ErrIdentityMismatch},
		{name: "zero", sequences: []uint64{1}, extra: lifetimeLaunchName(0), want: task.ErrEvidenceFault},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			td, digest := registeredCapacityTask(t)
			for _, sequence := range test.sequences {
				seedTaskLaunch(t, td, digest, sequence, "native:start")
			}
			if test.extra != "" {
				seedTaskLaunchFile(t, td, test.extra, nil)
			}
			if _, err := td.ReadLifetimeLaunches(); !errors.Is(err, test.want) {
				t.Fatalf("read error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestTaskLifetimeLaunchInventoryKeepsContextAndDirectoryFaults(t *testing.T) {
	td, digest := registeredCapacityTask(t)
	seedTaskLaunch(t, td, digest, 1, "native:start")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := td.ReadLifetimeLaunchesContext(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %v, want context.Canceled", err)
	}
	directory := filepath.Join(td.Dir, lifetimeDirectory)
	moved := directory + ".moved"
	must(t, os.Rename(directory, moved))
	must(t, os.Symlink(moved, directory))
	_, err := td.store.readLifetimeLaunchInventory(context.Background(), directory, taskLifetimeLaunchNaming())
	if err == nil || task.RecordAbsent(err) {
		t.Fatalf("symlinked lifetime directory = %v, want a non-absent fault", err)
	}
}

func newCapacityControlLifetime(t testing.TB) (*ControlDir, *ControlLifetime) {
	t.Helper()
	store, err := InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})
	return openCapacityControlLifetime(t, store, strings.Repeat("c", 32))
}

func openCapacityControlLifetime(t testing.TB, store *Store, operationID string) (*ControlDir, *ControlLifetime) {
	t.Helper()
	control, err := store.OpenWorkspaceOperation(context.Background(), operationID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Errorf("close control: %v", closeErr)
		}
	})
	deadline := time.Now().Add(time.Hour).UTC()
	lifetime, err := NewControlLifetime(control, store.RootID, operationID, task.LifetimeOwnerWorkspace, task.ComputeSHA256([]byte("capacity-owner")), deadline)
	if err != nil {
		t.Fatal(err)
	}
	return control, lifetime
}

func registerCapacityControlLifetime(ctx context.Context, t testing.TB, lifetime *ControlLifetime) string {
	t.Helper()
	deadline := lifetime.deadline
	if _, err := lifetime.RecordLifetimeIntentContext(ctx, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "control-capacity-plan", BootID: "control-boot", Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lifetime.RecordLifetimeRegistrationContext(ctx, LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopeID: "control-capacity-group", BootID: "control-boot", Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	digest, err := lifetime.LifetimeRegistrationDigestContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestControlLifetimeLaunchesPersistBeyondFormerLimit(t *testing.T) {
	_, lifetime := newCapacityControlLifetime(t)
	ctx := scenarioContext(t)
	registerCapacityControlLifetime(ctx, t, lifetime)
	count := uint64(formerLaunchLimit + 6)
	for sequence := uint64(1); sequence <= count; sequence++ {
		if _, err := lifetime.RecordLifetimeLaunchContext(ctx, LifetimeLaunchFacts{Sequence: sequence, PID: 1000 + int(sequence), ProcessStart: fmt.Sprintf("control-start-%d", sequence), PGID: 1000 + int(sequence)}); err != nil {
			t.Fatalf("record launch %d: %v", sequence, err)
		}
	}
	launches, err := lifetime.ReadLifetimeLaunchesContext(ctx)
	must(t, err)
	termination, err := lifetime.RecordLifetimeTerminationContext(ctx, task.LifetimeTerminationRecovery)
	must(t, err)
	digest, err := task.LifetimeLaunchSetDigest(launches)
	must(t, err)
	if uint64(len(launches)) != count || termination.LaunchCount != count || termination.LaunchSetSHA256 != digest {
		t.Fatalf("launches = %d, termination = %d %s, want %d %s", len(launches), termination.LaunchCount, termination.LaunchSetSHA256, count, digest)
	}
}

func putCapacityControlLaunch(t testing.TB, control *ControlDir, record task.LifetimeLaunchRecord, fileSequence uint64) {
	t.Helper()
	created, err := control.Put(controlLifetimeLaunchName(fileSequence), record)
	if err != nil || !created {
		t.Fatalf("seed control launch %d: created=%v err=%v", fileSequence, created, err)
	}
}

func controlCapacityLaunch(lifetime *ControlLifetime, digest string, sequence uint64) task.LifetimeLaunchRecord {
	return task.LifetimeLaunchRecord{
		SchemaVersion: task.SchemaVersion, RootID: lifetime.rootID, TaskID: lifetime.operationID,
		RegistrationSHA256: digest, Sequence: sequence, PID: 1000 + int(sequence),
		ProcessStart: "control-start", PGID: 1000 + int(sequence), StartedAt: timestamp(),
	}
}

// The orphan fence inventories every launch name: a record beyond the former
// 64-slot scan still refuses a fresh intent, and a name beyond the namespace
// is a fault rather than "no launch".
func TestControlLifetimeOrphanFenceSeesEveryInventoriedLaunch(t *testing.T) {
	t.Run("beyond former scan", func(t *testing.T) {
		control, lifetime := newCapacityControlLifetime(t)
		putCapacityControlLaunch(t, control, controlCapacityLaunch(lifetime, task.ComputeSHA256([]byte("orphan")), 100), 100)
		err := recordCapacityIntent(lifetime)
		if !errors.Is(err, task.ErrAlreadyStarted) {
			t.Fatalf("intent over orphaned launch 100 = %v, want ErrAlreadyStarted", err)
		}
	})
	t.Run("beyond namespace", func(t *testing.T) {
		control, lifetime := newCapacityControlLifetime(t)
		beyond := task.MaxLifetimeLaunches + 1
		putCapacityControlLaunch(t, control, controlCapacityLaunch(lifetime, task.ComputeSHA256([]byte("orphan")), beyond), beyond)
		err := recordCapacityIntent(lifetime)
		if !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, task.ErrAlreadyStarted) {
			t.Fatalf("intent over launch beyond namespace = %v, want evidence fault", err)
		}
	})
}

func recordCapacityIntent(lifetime *ControlLifetime) error {
	_, err := lifetime.RecordLifetimeIntentContext(context.Background(), LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "control-capacity-plan", BootID: "control-boot", Deadline: lifetime.deadline,
	})
	return err
}

// Canonical verification precedes capacity accounting: a decodable but
// non-canonical record keeps Verify's identity instead of being counted.
func TestControlLifetimeLaunchVerifyPrecedesCapacity(t *testing.T) {
	for name, mutate := range map[string]func([]byte) []byte{
		"extra whitespace": func(data []byte) []byte { return append([]byte(" "), data...) },
		"invalid utf8": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), "control-start", "control-\xffstart", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			control, lifetime := newCapacityControlLifetime(t)
			digest := registerCapacityControlLifetime(context.Background(), t, lifetime)
			data, err := task.MarshalCanonical(controlCapacityLaunch(lifetime, digest, 1))
			must(t, err)
			path := filepath.Join(control.dir, controlLifetimeLaunchName(1))
			must(t, os.WriteFile(path, mutate(data), 0o600))
			if _, err = lifetime.ReadLifetimeLaunches(); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("non-canonical launch = %v, want Verify evidence fault", err)
			}
		})
	}
}

func TestControlLifetimeLaunchInventoryRefusesGapAfterLaterRecord(t *testing.T) {
	control, lifetime := newCapacityControlLifetime(t)
	digest := registerCapacityControlLifetime(context.Background(), t, lifetime)
	putCapacityControlLaunch(t, control, controlCapacityLaunch(lifetime, digest, 1), 1)
	putCapacityControlLaunch(t, control, controlCapacityLaunch(lifetime, digest, 3), 3)
	if _, err := lifetime.ReadLifetimeLaunches(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("gap read = %v, want identity mismatch", err)
	}
	if _, err := lifetime.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: 2, PID: 1002, ProcessStart: "control-start", PGID: 1002}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("write into gap = %v, want identity mismatch", err)
	}
}

// The control writer adds a candidate once to the capacity its set reader
// accumulated: an idempotent rerecord at the exact byte limit is not counted
// twice, and a new record beyond it is refused before publication.
func TestControlLifetimeLaunchesEnforceAggregateBytesOnce(t *testing.T) {
	control, lifetime := newCapacityControlLifetime(t)
	digest := registerCapacityControlLifetime(context.Background(), t, lifetime)
	startedAt := timestamp()
	for sequence := uint64(1); sequence <= formerLaunchLimit; sequence++ {
		record := controlCapacityLaunch(lifetime, digest, sequence)
		record.StartedAt = startedAt
		base, err := task.MarshalCanonical(record)
		must(t, err)
		record.ProcessStart += strings.Repeat("x", task.MaxControlRecordSize-len(base))
		data, err := task.MarshalCanonical(record)
		must(t, err)
		if len(data) != task.MaxControlRecordSize {
			t.Fatalf("seed record size = %d, want %d", len(data), task.MaxControlRecordSize)
		}
		must(t, os.WriteFile(filepath.Join(control.dir, controlLifetimeLaunchName(sequence)), data, 0o600))
	}
	launches, err := lifetime.ReadLifetimeLaunches()
	must(t, err)
	last := launches[formerLaunchLimit-1]
	winner, err := lifetime.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: last.Sequence, PID: last.PID, ProcessStart: last.ProcessStart, PGID: last.PGID})
	if err != nil || winner.StartedAt != last.StartedAt {
		t.Fatalf("idempotent rerecord at the byte limit = %v, %v; want the existing winner", winner, err)
	}
	next := uint64(formerLaunchLimit + 1)
	_, err = lifetime.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: next, PID: 2000, ProcessStart: "control-start", PGID: 2000})
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("write beyond aggregate bytes = %v, want evidence fault", err)
	}
	if _, statErr := os.Stat(filepath.Join(control.dir, controlLifetimeLaunchName(next))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused launch record was published: %v", statErr)
	}
}
