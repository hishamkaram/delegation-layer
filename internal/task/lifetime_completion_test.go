package task

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	completionRootID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	completionTaskID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func completionIntent(bootID string) LifetimeIntentRecord {
	return LifetimeIntentRecord{
		SchemaVersion: SchemaVersion,
		RootID:        completionRootID,
		TaskID:        completionTaskID,
		OwnerKind:     LifetimeOwnerTask,
		OwnerSHA256:   ComputeSHA256([]byte("completion-owner")),
		ScopeKind:     LifetimeScopeCgroupV2,
		Assurance:     LifetimeAssuranceExact,
		ScopePlan:     "completion-scope-plan",
		BootID:        bootID,
		Deadline:      "2026-10-11T12:00:00Z",
		CreatedAt:     "2026-10-11T11:00:00Z",
	}
}

func completionRegistration(intent LifetimeIntentRecord) LifetimeRegistrationRecord {
	return LifetimeRegistrationRecord{
		SchemaVersion: SchemaVersion,
		RootID:        intent.RootID,
		TaskID:        intent.TaskID,
		OwnerKind:     intent.OwnerKind,
		OwnerSHA256:   intent.OwnerSHA256,
		ScopeKind:     intent.ScopeKind,
		Assurance:     intent.Assurance,
		ScopeID:       "completion-scope",
		BootID:        intent.BootID,
		Deadline:      intent.Deadline,
		CreatedAt:     "2026-10-11T11:01:00Z",
	}
}

func completionWorker(registration LifetimeRegistrationRecord) LifetimeWorkerRecord {
	return LifetimeWorkerRecord{
		SchemaVersion:      SchemaVersion,
		RootID:             registration.RootID,
		TaskID:             registration.TaskID,
		RegistrationSHA256: completionRegistrationHash(registration),
		PID:                321,
		ProcessStart:       "native:321",
		BootID:             registration.BootID,
		StartedAt:          "2026-10-11T11:02:00Z",
	}
}

func completionLaunches(registration LifetimeRegistrationRecord) []LifetimeLaunchRecord {
	return []LifetimeLaunchRecord{{
		SchemaVersion:      SchemaVersion,
		RootID:             registration.RootID,
		TaskID:             registration.TaskID,
		RegistrationSHA256: completionRegistrationHash(registration),
		Sequence:           1,
		PID:                654,
		ProcessStart:       "native:654",
		PGID:               654,
		StartedAt:          "2026-10-11T11:03:00Z",
	}}
}

func completionTermination(registration LifetimeRegistrationRecord, launches []LifetimeLaunchRecord, cause string) LifetimeTerminationRecord {
	return LifetimeTerminationRecord{
		SchemaVersion:      SchemaVersion,
		RootID:             registration.RootID,
		TaskID:             registration.TaskID,
		RegistrationSHA256: completionRegistrationHash(registration),
		LaunchCount:        uint64(len(launches)),
		LaunchSetSHA256:    completionLaunchSetHash(launches),
		Cause:              cause,
		ScopeEmpty:         true,
		ObservedAt:         "2026-10-11T11:04:00Z",
	}
}

func completionRegisteredEvidence(t *testing.T, withWorker, withLaunch bool, cause string) LifetimeEvidence {
	t.Helper()
	intent := completionIntent("boot-current")
	registration := completionRegistration(intent)
	var worker *LifetimeWorkerRecord
	if withWorker {
		value := completionWorker(registration)
		worker = &value
	}
	launches := make([]LifetimeLaunchRecord, 0)
	if withLaunch {
		launches = completionLaunches(registration)
	}
	termination := completionTermination(registration, launches, cause)
	return LifetimeEvidence{
		Intent:       &intent,
		Registration: &registration,
		Worker:       worker,
		Launches:     launches,
		Termination:  &termination,
	}
}

func completionRegistrationHash(record LifetimeRegistrationRecord) string {
	data, err := MarshalCanonical(record)
	if err != nil {
		panic(err)
	}
	return ComputeSHA256(data)
}

func completionWorkerHash(record LifetimeWorkerRecord) string {
	data, err := MarshalCanonical(record)
	if err != nil {
		panic(err)
	}
	return ComputeSHA256(data)
}

func completionLaunchSetHash(launches []LifetimeLaunchRecord) string {
	if launches == nil {
		launches = make([]LifetimeLaunchRecord, 0)
	}
	data, err := MarshalCanonical(launches)
	if err != nil {
		panic(err)
	}
	return ComputeSHA256(data)
}

func completionTime(hour, minute int) time.Time {
	return time.Date(2026, time.October, 11, hour, minute, 0, 0, time.UTC)
}

func completionExit(code int) *int {
	return &code
}

func TestLifetimeWorkerWaitNativeEncodingOmitsCompletionFields(t *testing.T) {
	intent := completionIntent("boot-current")
	registration := completionRegistration(intent)
	record := LifetimeWorkerWaitRecord{
		SchemaVersion:      SchemaVersion,
		RootID:             completionRootID,
		TaskID:             completionTaskID,
		RegistrationSHA256: completionRegistrationHash(registration),
		ChildPID:           321,
		Result:             LifetimeWorkerWaitSuccess,
		ExitCode:           completionExit(0),
		ObservedAt:         "2026-10-11T11:05:00Z",
	}
	if err := ValidateLifetimeWorkerWaitRecord(&record); err != nil {
		t.Fatal(err)
	}
	data, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"schema_version":1,"root_id":%q,"task_id":%q,"registration_sha256":%q,"child_pid":321,"result":"success","exit_code":0,"observed_at":"2026-10-11T11:05:00Z"}
`, completionRootID, completionTaskID, completionRegistrationHash(registration))
	if string(data) != want {
		t.Fatalf("native encoding changed:\n got %s\nwant %s", data, want)
	}
	for _, field := range []string{"intent_sha256", "absence_proof", "observed_boot_id"} {
		if strings.Contains(string(data), field) {
			t.Fatalf("native encoding contains optional completion field %q: %s", field, data)
		}
	}
}

func TestLifetimeCompletionBindingRegisteredAbsenceAndResolver(t *testing.T) {
	evidence := completionRegisteredEvidence(t, true, true, LifetimeTerminationCompleted)
	record, err := NewLifetimeWorkerAbsence(evidence, LifetimeAbsenceExactWorker, "boot-current", completionTime(11, 5))
	if err != nil {
		t.Fatal(err)
	}
	if record.RegistrationSHA256 == "" || record.IntentSHA256 != "" || record.ExitCode != nil {
		t.Fatalf("unexpected registered absence shape: %+v", record)
	}
	if LifetimeWorkerWaitIsQualified(&record) {
		t.Fatal("absence was treated as a qualified native Wait")
	}
	if !LifetimeWorkerWaitMatchesWorker(&record, evidence.Worker, record.WorkerSHA256) {
		t.Fatal("known worker identity did not bind to the absence witness")
	}
	evidence.WorkerWait = &record
	if err = ValidateLifetimeCompletionBinding(&record, evidence); err != nil {
		t.Fatal(err)
	}
	closure, err := ResolveLifetimeClosure(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureRegisteredRecovered || !closure.Closed() {
		t.Fatalf("closure = %+v, want registered recovered", closure)
	}

	badBoot := record
	badBoot.ObservedBootID = "boot-other"
	if err := ValidateLifetimeCompletionBinding(&badBoot, LifetimeEvidence{
		Intent:       evidence.Intent,
		Registration: evidence.Registration,
		Worker:       evidence.Worker,
		Launches:     evidence.Launches,
		Termination:  evidence.Termination,
	}); err == nil {
		t.Fatal("same-boot exact-worker absence accepted a mismatched boot")
	}
}

func TestLifetimeCompletionBindingIntentAbsenceVariants(t *testing.T) {
	intent := completionIntent("boot-current")
	for _, test := range []struct {
		name         string
		proof        LifetimeAbsenceProof
		observedBoot string
	}{
		{name: "disposed", proof: LifetimeAbsenceUnregisteredDisposed, observedBoot: "boot-current"},
		{name: "previous-boot", proof: LifetimeAbsencePreviousBoot, observedBoot: "boot-previous"},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := LifetimeEvidence{Intent: &intent}
			record, err := NewLifetimeWorkerAbsence(evidence, test.proof, test.observedBoot, completionTime(11, 2))
			if err != nil {
				t.Fatal(err)
			}
			if record.RegistrationSHA256 != "" || record.IntentSHA256 == "" || record.WorkerSHA256 != "" || record.ChildPID != 0 || record.ExitCode != nil {
				t.Fatalf("unexpected intent absence shape: %+v", record)
			}
			evidence.WorkerWait = &record
			closure, resolveErr := ResolveLifetimeClosure(evidence)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if closure.Kind != LifetimeClosureUnregisteredRecovered || !closure.Closed() {
				t.Fatalf("closure = %+v, want unregistered recovered", closure)
			}
		})
	}
}

func TestLifetimeCompletionResolverPreservesPreviousBootEvidence(t *testing.T) {
	evidence := completionRegisteredEvidence(t, false, false, LifetimeTerminationCompleted)
	record, err := NewLifetimeWorkerAbsence(evidence, LifetimeAbsencePreviousBoot, "boot-next", completionTime(11, 5))
	if err != nil {
		t.Fatal(err)
	}
	evidence.WorkerWait = &record
	closure, err := ResolveLifetimeClosure(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureRegisteredRecovered {
		t.Fatalf("closure = %+v, want registered recovered", closure)
	}

	legacy := evidence
	legacy.WorkerWait = nil
	legacy.Termination.Cause = LifetimeTerminationPreviousBoot
	closure, err = ResolveLifetimeClosure(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureLegacyPreviousBoot || !closure.Closed() {
		t.Fatalf("legacy closure = %+v, want legacy previous boot", closure)
	}
}

func TestLifetimeCompletionResolverDoesNotInferScopeEmptyWorkerExit(t *testing.T) {
	evidence := completionRegisteredEvidence(t, false, false, LifetimeTerminationCompleted)
	evidence.Termination = nil
	closure, err := ResolveLifetimeClosure(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureOpen || closure.Closed() {
		t.Fatalf("closure = %+v, want open without a termination witness", closure)
	}

	evidence = completionRegisteredEvidence(t, false, false, LifetimeTerminationCompleted)
	closure, err = ResolveLifetimeClosure(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureOpen || closure.Closed() {
		t.Fatalf("closure = %+v, want open without a completion disposition", closure)
	}
}

func TestLifetimeCompletionResolverNativeWaitRequiresTerminationForClosure(t *testing.T) {
	evidence := completionRegisteredEvidence(t, true, false, LifetimeTerminationCompleted)
	wait := LifetimeWorkerWaitRecord{
		SchemaVersion:      SchemaVersion,
		RootID:             completionRootID,
		TaskID:             completionTaskID,
		RegistrationSHA256: completionRegistrationHash(*evidence.Registration),
		WorkerSHA256:       completionWorkerHash(*evidence.Worker),
		ChildPID:           evidence.Worker.PID,
		Result:             LifetimeWorkerWaitSuccess,
		ExitCode:           completionExit(0),
		ObservedAt:         "2026-10-11T11:05:00Z",
	}
	evidence.WorkerWait = &wait
	evidence.Termination = nil
	closure, err := ResolveLifetimeClosure(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureOpen {
		t.Fatalf("closure = %+v, want open", closure)
	}
}

func TestLifetimeCompletionFaultsContradictoryFacts(t *testing.T) {
	baseEvidence := completionRegisteredEvidence(t, true, true, LifetimeTerminationCompleted)
	baseRecord, err := NewLifetimeWorkerAbsence(baseEvidence, LifetimeAbsenceExactWorker, "boot-current", completionTime(11, 5))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		make func() LifetimeEvidence
	}{
		{
			name: "registration-hash",
			make: func() LifetimeEvidence {
				evidence := baseEvidence
				record := baseRecord
				record.RegistrationSHA256 = ComputeSHA256([]byte("wrong-registration"))
				evidence.WorkerWait = &record
				return evidence
			},
		},
		{
			name: "root",
			make: func() LifetimeEvidence {
				evidence := baseEvidence
				record := baseRecord
				record.RootID = strings.Repeat("c", 32)
				evidence.WorkerWait = &record
				return evidence
			},
		},
		{
			name: "worker-task",
			make: func() LifetimeEvidence {
				evidence := baseEvidence
				worker := *evidence.Worker
				worker.TaskID = strings.Repeat("d", 32)
				evidence.Worker = &worker
				evidence.WorkerWait = &baseRecord
				return evidence
			},
		},
		{
			name: "proof-boot",
			make: func() LifetimeEvidence {
				evidence := baseEvidence
				record := baseRecord
				record.AbsenceProof = LifetimeAbsencePreviousBoot
				evidence.WorkerWait = &record
				return evidence
			},
		},
		{
			name: "exit-code",
			make: func() LifetimeEvidence {
				evidence := baseEvidence
				record := baseRecord
				record.ExitCode = completionExit(0)
				evidence.WorkerWait = &record
				return evidence
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, resolveErr := ResolveLifetimeClosure(test.make())
			if !errors.Is(resolveErr, ErrEvidenceFault) {
				t.Fatalf("ResolveLifetimeClosure error = %v, want ErrEvidenceFault", resolveErr)
			}
		})
	}
}

func TestLifetimeCompletionRejectsMissingWorkerForExactWorker(t *testing.T) {
	evidence := completionRegisteredEvidence(t, false, false, LifetimeTerminationCompleted)
	if _, err := NewLifetimeWorkerAbsence(evidence, LifetimeAbsenceExactWorker, "boot-current", completionTime(11, 5)); err == nil {
		t.Fatal("exact-worker absence accepted without a worker record")
	}
}

func TestLifetimeCompletionRejectsUnqualifiedRegisteredScopeEmptyAbsence(t *testing.T) {
	evidence := completionRegisteredEvidence(t, false, false, LifetimeTerminationCompleted)
	if _, err := NewLifetimeWorkerAbsence(evidence, LifetimeAbsenceExactWorker, "boot-current", completionTime(11, 5)); err == nil {
		t.Fatal("registered scope-empty evidence inferred exact worker absence")
	}
}

func TestLifetimeCompletionPartialFactsRemainOpen(t *testing.T) {
	intent := completionIntent("boot-current")
	closure, err := ResolveLifetimeClosure(LifetimeEvidence{Intent: &intent})
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureOpen {
		t.Fatalf("intent-only closure = %+v, want open", closure)
	}

	evidence := completionRegisteredEvidence(t, true, false, LifetimeTerminationCompleted)
	wait := LifetimeWorkerWaitRecord{
		SchemaVersion:      SchemaVersion,
		RootID:             completionRootID,
		TaskID:             completionTaskID,
		RegistrationSHA256: completionRegistrationHash(*evidence.Registration),
		WorkerSHA256:       completionWorkerHash(*evidence.Worker),
		ChildPID:           evidence.Worker.PID,
		Result:             LifetimeWorkerWaitSuccess,
		ExitCode:           completionExit(0),
		ObservedAt:         "2026-10-11T11:05:00Z",
	}
	evidence.WorkerWait = &wait
	evidence.Termination = nil
	closure, err = ResolveLifetimeClosure(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Kind != LifetimeClosureOpen {
		t.Fatalf("native wait without termination = %+v, want open", closure)
	}
}

func TestLifetimeCompletionMalformedFactsAreEvidenceFaults(t *testing.T) {
	intent := completionIntent("boot-current")
	intent.CreatedAt = "not-a-timestamp"
	_, err := ResolveLifetimeClosure(LifetimeEvidence{Intent: &intent})
	if !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("ResolveLifetimeClosure error = %v, want ErrEvidenceFault", err)
	}

	evidence := completionRegisteredEvidence(t, true, true, LifetimeTerminationCompleted)
	evidence.Termination.LaunchCount++
	_, err = ResolveLifetimeClosure(evidence)
	if !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("ResolveLifetimeClosure error = %v, want ErrEvidenceFault", err)
	}
}

// Existing record validation accepts RFC3339 offsets. Recovery does not tighten
// the native record format or reinterpret immutable older evidence.
func TestLifetimeCompletionPreservesAcceptedNativeTimestampFormat(t *testing.T) {
	intent := completionIntent("boot-current")
	intent.CreatedAt = "2026-10-11T12:00:00+01:00"
	closure, err := ResolveLifetimeClosure(LifetimeEvidence{Intent: &intent})
	if err != nil || closure.Closed() {
		t.Fatalf("valid old intent: closure=%+v err=%v", closure, err)
	}
}

func TestLifetimeCompletionRejectsOrphanedDependentFacts(t *testing.T) {
	evidence := completionRegisteredEvidence(t, true, true, LifetimeTerminationCompleted)
	for _, test := range []struct {
		name     string
		evidence LifetimeEvidence
	}{
		{"worker", LifetimeEvidence{Worker: evidence.Worker}},
		{"launch", LifetimeEvidence{Launches: evidence.Launches}},
		{"termination", LifetimeEvidence{Termination: evidence.Termination}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveLifetimeClosure(test.evidence); !errors.Is(err, ErrEvidenceFault) {
				t.Fatalf("orphaned dependent: %v", err)
			}
		})
	}
}

func TestLifetimeCompletionRecoveryDoesNotDependOnWallClockOrder(t *testing.T) {
	evidence := completionRegisteredEvidence(t, true, true, LifetimeTerminationCompleted)
	observedAt, err := time.Parse(time.RFC3339Nano, "2026-10-11T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	completion, err := NewLifetimeWorkerAbsence(evidence, LifetimeAbsenceExactWorker, evidence.Registration.BootID, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	evidence.WorkerWait = &completion
	closure, err := ResolveLifetimeClosure(evidence)
	if err != nil || !closure.Closed() {
		t.Fatalf("positive native witness after clock rollback: closure=%+v err=%v", closure, err)
	}
}
