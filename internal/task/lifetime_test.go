package task

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRecordAbsentRequiresUnmarkedFilesystemAbsence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "direct", err: os.ErrNotExist, want: true},
		{name: "identity", err: errors.Join(ErrIdentityMismatch, os.ErrNotExist)},
		{name: "evidence", err: errors.Join(ErrEvidenceFault, os.ErrNotExist)},
		{name: "canceled", err: errors.Join(context.Canceled, os.ErrNotExist)},
		{name: "deadline", err: errors.Join(context.DeadlineExceeded, os.ErrNotExist)},
		{name: "nil", err: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecordAbsent(tc.err); got != tc.want {
				t.Fatalf("RecordAbsent(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func validLifetimeRegistration() LifetimeRegistrationRecord {
	return LifetimeRegistrationRecord{
		SchemaVersion: SchemaVersion,
		RootID:        strings.Repeat("1", 32),
		TaskID:        strings.Repeat("2", 32),
		OwnerKind:     LifetimeOwnerTask,
		OwnerSHA256:   ComputeSHA256([]byte("owner")),
		ScopeKind:     LifetimeScopeCgroupV2,
		Assurance:     LifetimeAssuranceExact,
		ScopeID:       "/sys/fs/cgroup/delegate-test",
		BootID:        "boot-token",
		Deadline:      "2026-10-11T12:00:00Z",
		CreatedAt:     "2026-10-11T11:00:00Z",
	}
}

func validLifetimeIntent() LifetimeIntentRecord {
	return LifetimeIntentRecord{
		SchemaVersion: SchemaVersion,
		RootID:        strings.Repeat("1", 32),
		TaskID:        strings.Repeat("2", 32),
		OwnerKind:     LifetimeOwnerTask,
		OwnerSHA256:   ComputeSHA256([]byte("owner")),
		ScopeKind:     LifetimeScopeCgroupV2,
		Assurance:     LifetimeAssuranceExact,
		ScopePlan:     "cgroup-plan-v1",
		BootID:        "boot-token",
		Deadline:      "2026-10-11T12:00:00Z",
		CreatedAt:     "2026-10-11T11:00:00Z",
	}
}

func validLifetimeWorker() LifetimeWorkerRecord {
	return LifetimeWorkerRecord{
		SchemaVersion:      SchemaVersion,
		RootID:             strings.Repeat("1", 32),
		TaskID:             strings.Repeat("2", 32),
		RegistrationSHA256: ComputeSHA256([]byte("registration")),
		PID:                123,
		ProcessStart:       "native:456",
		BootID:             "boot-token",
		StartedAt:          "2026-10-11T11:00:00Z",
	}
}

func validLifetimeWorkerWait() LifetimeWorkerWaitRecord {
	exitCode := 0
	return LifetimeWorkerWaitRecord{
		SchemaVersion:      SchemaVersion,
		RootID:             strings.Repeat("1", 32),
		TaskID:             strings.Repeat("2", 32),
		RegistrationSHA256: ComputeSHA256([]byte("registration")),
		ChildPID:           123,
		Result:             LifetimeWorkerWaitSuccess,
		ExitCode:           &exitCode,
		ObservedAt:         "2026-10-11T11:00:00Z",
	}
}

func TestLifetimeIntentAndWorkerValidation(t *testing.T) {
	intent := validLifetimeIntent()
	if err := ValidateLifetimeIntentRecord(&intent); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*LifetimeIntentRecord){
		func(r *LifetimeIntentRecord) { r.OwnerKind = "unknown" },
		func(r *LifetimeIntentRecord) { r.OwnerSHA256 = "invalid" },
		func(r *LifetimeIntentRecord) { r.ScopeKind = LifetimeScopeProcessGroup },
		func(r *LifetimeIntentRecord) { r.Assurance = LifetimeAssuranceCooperative },
		func(r *LifetimeIntentRecord) { r.ScopePlan = "" },
		func(r *LifetimeIntentRecord) { r.BootID = "" },
		func(r *LifetimeIntentRecord) { r.Deadline = "invalid" },
		func(r *LifetimeIntentRecord) { r.CreatedAt = "invalid" },
	} {
		record := intent
		mutate(&record)
		if err := ValidateLifetimeIntentRecord(&record); err == nil {
			t.Fatal("invalid lifetime intent accepted")
		}
	}
	worker := validLifetimeWorker()
	if err := ValidateLifetimeWorkerRecord(&worker); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*LifetimeWorkerRecord){
		func(r *LifetimeWorkerRecord) { r.RegistrationSHA256 = "invalid" },
		func(r *LifetimeWorkerRecord) { r.PID = 0 },
		func(r *LifetimeWorkerRecord) { r.ProcessStart = "" },
		func(r *LifetimeWorkerRecord) { r.BootID = "" },
		func(r *LifetimeWorkerRecord) { r.StartedAt = "2026-10-11T12:00:00+01:00" },
		func(r *LifetimeWorkerRecord) { r.StartedAt = "invalid" },
	} {
		record := worker
		mutate(&record)
		if err := ValidateLifetimeWorkerRecord(&record); err == nil {
			t.Fatal("invalid lifetime worker accepted")
		}
	}
}

func TestLifetimeWorkerWaitValidationCoversResultsAndBoundaries(t *testing.T) {
	for _, test := range []struct {
		name     string
		result   LifetimeWorkerWaitResult
		exitCode *int
	}{
		{name: "success", result: LifetimeWorkerWaitSuccess, exitCode: intPointer(0)},
		{name: "exit-error", result: LifetimeWorkerWaitExitError, exitCode: intPointer(-1)},
		{name: "wait-error", result: LifetimeWorkerWaitError},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := validLifetimeWorkerWait()
			record.Result = test.result
			record.ExitCode = test.exitCode
			if err := ValidateLifetimeWorkerWaitRecord(&record); err != nil {
				t.Fatal(err)
			}
		})
	}

	valid := validLifetimeWorkerWait()
	for _, mutate := range []func(*LifetimeWorkerWaitRecord){
		func(r *LifetimeWorkerWaitRecord) { r.RegistrationSHA256 = "invalid" },
		func(r *LifetimeWorkerWaitRecord) { r.ChildPID = 0 },
		func(r *LifetimeWorkerWaitRecord) { r.Result = "unknown" },
		func(r *LifetimeWorkerWaitRecord) { r.Result = LifetimeWorkerWaitSuccess; r.ExitCode = nil },
		func(r *LifetimeWorkerWaitRecord) { r.Result = LifetimeWorkerWaitSuccess; r.ExitCode = intPointer(1) },
		func(r *LifetimeWorkerWaitRecord) { r.Result = LifetimeWorkerWaitExitError; r.ExitCode = nil },
		func(r *LifetimeWorkerWaitRecord) { r.Result = LifetimeWorkerWaitExitError; r.ExitCode = intPointer(0) },
		func(r *LifetimeWorkerWaitRecord) { r.ObservedAt = "2026-10-11T12:00:00+01:00" },
	} {
		record := valid
		mutate(&record)
		if err := ValidateLifetimeWorkerWaitRecord(&record); err == nil {
			t.Fatal("invalid lifetime worker wait accepted")
		}
	}
}

func TestLifetimeWorkerWaitNotStartedHasNoProcessIdentity(t *testing.T) {
	record := validLifetimeWorkerWait()
	record.ChildPID = 0
	record.Result = LifetimeWorkerWaitNotStarted
	record.ExitCode = nil
	if err := ValidateLifetimeWorkerWaitRecord(&record); err != nil {
		t.Fatal(err)
	}
	if !LifetimeWorkerWaitProvesNotStarted(&record) {
		t.Fatal("not-started disposition was not recognized")
	}

	for _, mutate := range []func(*LifetimeWorkerWaitRecord){
		func(r *LifetimeWorkerWaitRecord) { r.ChildPID = 1 },
		func(r *LifetimeWorkerWaitRecord) { r.WorkerSHA256 = ComputeSHA256([]byte("worker")) },
		func(r *LifetimeWorkerWaitRecord) { r.ExitCode = intPointer(0) },
	} {
		invalid := record
		mutate(&invalid)
		if err := ValidateLifetimeWorkerWaitRecord(&invalid); err == nil {
			t.Fatal("not-started disposition accepted process identity")
		}
	}
}

func TestLifetimeWorkerWaitRelationshipsDistinguishQualifiedProof(t *testing.T) {
	registration := validLifetimeRegistration()
	registrationDigest := ComputeSHA256([]byte("registration"))
	worker := validLifetimeWorker()
	workerDigest := ComputeSHA256([]byte("worker"))
	qualified := validLifetimeWorkerWait()
	qualified.WorkerSHA256 = workerDigest
	if !LifetimeWorkerWaitMatchesRegistration(&qualified, &registration, registrationDigest) {
		t.Fatal("qualified wait did not match registration")
	}
	if !LifetimeWorkerWaitMatchesWorker(&qualified, &worker, workerDigest) {
		t.Fatal("qualified wait did not match worker")
	}
	if !LifetimeWorkerWaitIsQualified(&qualified) {
		t.Fatal("qualified wait was not identified as qualified")
	}

	unqualified := validLifetimeWorkerWait()
	if LifetimeWorkerWaitIsQualified(&unqualified) {
		t.Fatal("unqualified wait was identified as qualified")
	}
	if LifetimeWorkerWaitMatchesWorker(&unqualified, &worker, workerDigest) {
		t.Fatal("unqualified wait matched a worker")
	}
	wrongPID := qualified
	wrongPID.ChildPID++
	if LifetimeWorkerWaitMatchesWorker(&wrongPID, &worker, workerDigest) {
		t.Fatal("wait with wrong PID matched a worker")
	}
	wrongRegistration := qualified
	wrongRegistration.RegistrationSHA256 = ComputeSHA256([]byte("other-registration"))
	if LifetimeWorkerWaitMatchesRegistration(&wrongRegistration, &registration, registrationDigest) {
		t.Fatal("wait with wrong registration matched a registration")
	}
}

func intPointer(value int) *int {
	return &value
}

func TestLifetimeRegistrationRequiresMatchingAssurance(t *testing.T) {
	valid := validLifetimeRegistration()
	if err := ValidateLifetimeRegistrationRecord(&valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*LifetimeRegistrationRecord){
		func(r *LifetimeRegistrationRecord) { r.Assurance = LifetimeAssuranceCooperative },
		func(r *LifetimeRegistrationRecord) { r.ScopeKind = LifetimeScopeProcessGroup },
		func(r *LifetimeRegistrationRecord) { r.ScopeID = "" },
		func(r *LifetimeRegistrationRecord) { r.BootID = "" },
		func(r *LifetimeRegistrationRecord) { r.Deadline = "invalid" },
		func(r *LifetimeRegistrationRecord) { r.OwnerKind = "unknown" },
		func(r *LifetimeRegistrationRecord) { r.OwnerSHA256 = "invalid" },
	} {
		record := valid
		mutate(&record)
		if err := ValidateLifetimeRegistrationRecord(&record); err == nil {
			t.Fatal("invalid lifetime registration accepted")
		}
	}
	cooperative := valid
	cooperative.ScopeKind = LifetimeScopeProcessGroup
	cooperative.Assurance = LifetimeAssuranceCooperative
	if err := ValidateLifetimeRegistrationRecord(&cooperative); err != nil {
		t.Fatalf("valid cooperative registration rejected: %v", err)
	}
}

func TestLifetimeLaunchAndTerminationValidation(t *testing.T) {
	registration := validLifetimeRegistration()
	launch := LifetimeLaunchRecord{
		SchemaVersion: registration.SchemaVersion,
		RootID:        registration.RootID, TaskID: registration.TaskID,
		RegistrationSHA256: ComputeSHA256([]byte("registration")), Sequence: 1,
		PID: 123, ProcessStart: "native:456", PGID: 123, StartedAt: registration.CreatedAt,
	}
	if err := ValidateLifetimeLaunchRecord(&launch); err != nil {
		t.Fatal(err)
	}
	invalidLaunch := launch
	invalidLaunch.Sequence = 0
	if err := ValidateLifetimeLaunchRecord(&invalidLaunch); err == nil {
		t.Fatal("zero launch sequence accepted")
	}
	termination := LifetimeTerminationRecord{
		SchemaVersion: registration.SchemaVersion,
		RootID:        registration.RootID, TaskID: registration.TaskID,
		RegistrationSHA256: launch.RegistrationSHA256,
		LaunchCount:        1, LaunchSetSHA256: ComputeSHA256([]byte("launches")),
		Cause: LifetimeTerminationBudget, ScopeEmpty: true, ObservedAt: registration.CreatedAt,
	}
	if err := ValidateLifetimeTerminationRecord(&termination); err != nil {
		t.Fatal(err)
	}
	termination.Cause = LifetimeTerminationRequested
	if err := ValidateLifetimeTerminationRecord(&termination); err != nil {
		t.Fatal(err)
	}
	termination.ScopeEmpty = false
	if err := ValidateLifetimeTerminationRecord(&termination); err == nil {
		t.Fatal("nonempty lifetime termination accepted")
	}
}

func TestLifetimeTaskOwnerSHA256IsDomainSeparated(t *testing.T) {
	spec := ComputeSHA256([]byte("spec"))
	meta := ComputeSHA256([]byte("meta"))
	got, err := LifetimeTaskOwnerSHA256(spec, meta)
	if err != nil {
		t.Fatal(err)
	}
	if got == spec || got == meta {
		t.Fatalf("owner digest was not domain separated: %q", got)
	}
	if _, err = LifetimeTaskOwnerSHA256("invalid", meta); err == nil {
		t.Fatal("invalid task owner digest input accepted")
	}
}
