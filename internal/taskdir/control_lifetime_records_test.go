package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestControlLifetimeRecordsPreserveCanonicalBindings(t *testing.T) {
	store := testStore(t)
	operationID := strings.Repeat("e", 32)
	control, err := store.OpenInspection(operationID, true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })

	deadline := time.Now().UTC().Add(time.Hour)
	ownerSHA256 := task.ComputeSHA256([]byte("control-lifetime-owner"))
	lifetime, err := NewControlLifetime(control, store.RootID, operationID, task.LifetimeOwnerInspection, ownerSHA256, deadline)
	must(t, err)
	requireControlLifetimeOwnerValidation(t, control, store.RootID, operationID, ownerSHA256, deadline)

	intentFacts := LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "control-scope-plan-v1",
		BootID:    "control-boot-v1",
		Deadline:  deadline,
	}
	intent, err := lifetime.RecordLifetimeIntentContext(t.Context(), intentFacts)
	must(t, err)
	registration, err := lifetime.RecordLifetimeRegistrationContext(t.Context(), LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "control-process-group",
		BootID:    intentFacts.BootID,
		Deadline:  deadline,
	})
	must(t, err)
	requireControlLifetimeRegistrationBinding(t, registration, store.RootID, operationID, ownerSHA256, deadline)
	_, err = lifetime.ReadLifetimeIntentContext(t.Context())
	must(t, err)

	worker, err := lifetime.RecordLifetimeWorkerContext(t.Context(), LifetimeWorkerFacts{
		PID:          2101,
		ProcessStart: "control-worker-start",
		BootID:       intentFacts.BootID,
	})
	must(t, err)
	workerDigest := digestControlLifetimeRecord(t, worker)
	_, err = lifetime.RecordLifetimeWorkerWaitContext(t.Context(), LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest,
		ChildPID:     worker.PID,
		Result:       task.LifetimeWorkerWaitSuccess,
		ExitCode:     controlLifetimeIntPointer(0),
	})
	must(t, err)

	launch, err := lifetime.RecordLifetimeLaunchContext(t.Context(), LifetimeLaunchFacts{
		Sequence:     1,
		PID:          2102,
		ProcessStart: "control-launch-start",
		PGID:         2102,
	})
	must(t, err)
	requireControlLifetimeLaunchBinding(t, launch, store.RootID, operationID)
	registrationDigest, err := lifetime.LifetimeRegistrationDigestContext(t.Context())
	must(t, err)
	requireControlLifetimeRegistrationDigest(t, registrationDigest, registration)

	termination, err := lifetime.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationBudget)
	must(t, err)
	requireControlLifetimeTermination(t, termination)
	has, err := lifetime.HasTerminatedLifetimeContext(t.Context(), task.LifetimeTerminationBudget)
	must(t, err)
	requireControlLifetimeTerminationPresent(t, has)
	_, err = lifetime.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: 2, PID: 2103, ProcessStart: "after-termination", PGID: 2103})
	if !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("launch after termination error = %v, want terminal task", err)
	}
	requireControlLifetimeRecordNames(t, control)
	if intent.TaskID != operationID {
		t.Fatalf("intent operation binding = %q, want %q", intent.TaskID, operationID)
	}
}

func requireControlLifetimeOwnerValidation(t *testing.T, control *ControlDir, rootID, operationID, ownerSHA256 string, deadline time.Time) {
	t.Helper()
	_, err := NewControlLifetime(control, rootID, operationID, "unknown-owner", ownerSHA256, deadline)
	if !errors.Is(err, task.ErrInvalidEnum) {
		t.Fatalf("invalid owner error = %v, want invalid enum", err)
	}
	if _, err = NewControlLifetime(control, rootID, operationID, task.LifetimeOwnerWorkspace, ownerSHA256, deadline); err != nil {
		t.Fatalf("workspace owner error = %v, want accepted owner kind", err)
	}
}

func requireControlLifetimeRegistrationBinding(t *testing.T, record *task.LifetimeRegistrationRecord, rootID, operationID, ownerSHA256 string, deadline time.Time) {
	t.Helper()
	if record.RootID != rootID || record.TaskID != operationID || record.OwnerKind != task.LifetimeOwnerInspection || record.OwnerSHA256 != ownerSHA256 || record.Deadline != deadline.Format(time.RFC3339Nano) {
		t.Fatalf("registration binding = %+v", record)
	}
}

func requireControlLifetimeLaunchBinding(t *testing.T, record *task.LifetimeLaunchRecord, rootID, operationID string) {
	t.Helper()
	if record.RootID != rootID || record.TaskID != operationID || record.RegistrationSHA256 == "" {
		t.Fatalf("launch binding = %+v", record)
	}
}

func requireControlLifetimeRegistrationDigest(t *testing.T, digest string, record *task.LifetimeRegistrationRecord) {
	t.Helper()
	want := digestControlLifetimeRecord(t, record)
	if digest != want {
		t.Fatalf("registration digest = %q, want %q", digest, want)
	}
}

func requireControlLifetimeTermination(t *testing.T, record *task.LifetimeTerminationRecord) {
	t.Helper()
	if record.LaunchCount != 1 || !record.ScopeEmpty {
		t.Fatalf("termination = %+v", record)
	}
}

func requireControlLifetimeTerminationPresent(t *testing.T, present bool) {
	t.Helper()
	if !present {
		t.Fatal("generic control lifetime did not report termination")
	}
}

func requireControlLifetimeRecordNames(t *testing.T, control *ControlDir) {
	t.Helper()
	for _, name := range []string{
		controlLifetimeIntentName,
		controlLifetimeRegistrationName,
		controlLifetimeWorkerName,
		controlLifetimeWorkerWaitName,
		controlLifetimeLaunchName(1),
		controlLifetimeTerminationName,
	} {
		if _, err := os.Stat(filepath.Join(control.dir, name)); err != nil {
			t.Fatalf("canonical record %q missing: %v", name, err)
		}
	}
}

func digestControlLifetimeRecord[T interface {
	task.LifetimeRegistrationRecord | *task.LifetimeRegistrationRecord | task.LifetimeWorkerRecord | *task.LifetimeWorkerRecord
}](t *testing.T, record T) string {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	must(t, err)
	return task.ComputeSHA256(data)
}

func controlLifetimeIntPointer(value int) *int {
	return &value
}
