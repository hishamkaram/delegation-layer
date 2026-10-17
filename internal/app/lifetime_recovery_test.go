package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRecoverTaskLifetimeRefusesCooperativeScopeWithoutLaunchIdentity(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, admissionfixture.CurrentBootID(t))

	if err := guardian.RecoverTaskLifetime(t.Context(), td); !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("recovery error = %v, want unverifiable launch", err)
	}
	if _, err := td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cooperative scope received false termination evidence: %v", err)
	}
}

func TestRecoverTaskLifetimeDoesNotIgnorePresentRecordsWithMissingPreparedInput(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, admissionfixture.CurrentBootID(t))
	if err := os.Remove(filepath.Join(td.Dir, "brief.md")); err != nil {
		t.Fatal(err)
	}

	err := guardian.RecoverTaskLifetime(t.Context(), td)
	if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func TestRecoverTaskLifetimeLeavesActiveOwnerUntouched(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, admissionfixture.DifferentNativeBootID(t))
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	if err = guardian.RecoverTaskLifetime(t.Context(), td); !errors.Is(err, guardian.ErrRecoveryPending) {
		t.Fatalf("recovery error = %v, want pending", err)
	}
	if _, err = td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active owner was overwritten: %v", err)
	}
}

func TestUpdateStatusKeepsIntentOnlyLifetimeRunningWhileOwnerBusy(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	_, err := td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("d", 32),
		BootID:    admissionfixture.CurrentBootID(t),
		Deadline:  time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err = updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	if response.Liveness != task.LivenessRunning.String() {
		t.Fatalf("liveness = %q, want running while intent owner is active", response.Liveness)
	}
	if response.Pending == nil || response.Pending.Kind != pendingLifetimeFinalization {
		t.Fatalf("pending = %+v, want lifetime finalization", response.Pending)
	}
}

func TestRecoverTaskLifetimeAcceptsChangedBootAsEmptyProof(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, admissionfixture.DifferentNativeBootID(t))

	if err := guardian.RecoverTaskLifetime(t.Context(), td); err != nil {
		t.Fatal(err)
	}
	terminated, err := td.HasTerminatedLifetime(task.LifetimeTerminationPreviousBoot)
	if err != nil || !terminated {
		t.Fatalf("changed boot recovery = (%t, %v)", terminated, err)
	}
}

func TestRecoverTaskLifetimeCleansUnregisteredIntentWithoutTerminationEvidence(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	deadline := time.Now().Add(time.Minute)
	_, err := td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("b", 32),
		BootID:    admissionfixture.CurrentBootID(t),
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = guardian.RecoverTaskLifetime(t.Context(), td); err != nil {
		t.Fatalf("recover unregistered intent: %v", err)
	}
	if _, err = td.ReadLifetimeRegistration(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered intent fabricated registration: %v", err)
	}
	if _, err = td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered intent fabricated termination: %v", err)
	}
}

func TestRecoverTaskLifetimeBeforeWorkerAcknowledgementRefusesWithoutTerminalProof(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	deadline := time.Now().Add(time.Minute)
	bootID := admissionfixture.CurrentBootID(t)
	plan := "pg:" + strings.Repeat("c", 32)
	if _, err := td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: plan,
		BootID:    bootID,
		Deadline:  deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := td.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   plan,
		BootID:    bootID,
		Deadline:  deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := guardian.RecoverTaskLifetime(t.Context(), td); !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("recover pre-ack registration = %v, want ErrUnverifiableLaunch", err)
	}
	if _, err := td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-ack recovery wrote terminal evidence: %v", err)
	}
}

func TestUpdateStatusKeepsRegisteredLifetimeRunningWhileOwnerBusy(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, admissionfixture.DifferentNativeBootID(t))
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err = updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	if response.Liveness != task.LivenessRunning.String() {
		t.Fatalf("liveness = %q, want running while lifetime owner is active", response.Liveness)
	}
	if response.Pending == nil || response.Pending.Kind != pendingLifetimeFinalization {
		t.Fatalf("pending = %+v, want lifetime finalization", response.Pending)
	}
	if _, err = td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy lifetime received false termination evidence: %v", err)
	}
}

func TestUpdateStatusKeepsUnprovenRegisteredLifetimeRunning(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, admissionfixture.CurrentBootID(t))

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	err := updateStatusStopsContext(t.Context(), td, &response, observation)
	if !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("status recovery error = %v, want unverifiable launch", err)
	}
	if response.Liveness != task.LivenessRunning.String() {
		t.Fatalf("liveness = %q, want running without lifetime termination proof", response.Liveness)
	}
}

func TestUpdateStatusKeepsHistoricalEndedCompatibility(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err := updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	if response.Liveness != task.LivenessEnded.String() {
		t.Fatalf("historical liveness = %q, want ended", response.Liveness)
	}
}

func TestUpdateStatusRecoversLifetimeBeforePublishingStop(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	prepareAppBudgetRecords(t, td)
	recordRecoverableLifetime(t, td, admissionfixture.DifferentNativeBootID(t))
	deadline := time.Now().Add(time.Minute)
	permit, err := td.PrepareStop("budget", "budget", deadline)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStopReply("budget", task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err = updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Observation == nil || !records[0].Observation.Terminated {
		t.Fatalf("stop was not published after lifetime recovery: %+v", records)
	}
	if response.Liveness != task.LivenessEnded.String() {
		t.Fatalf("liveness = %q, want ended after lifetime recovery", response.Liveness)
	}
}

func recordRecoverableLifetime(t *testing.T, td *taskdir.TaskDir, bootID string) {
	t.Helper()
	lifecyclefixture.RecordRecoverableLifetime(t, td, bootID)
}

func TestRecoverTaskLifetimeRejectsUnknownBootWithoutClosing(t *testing.T) {
	for _, boot := range []string{"different-boot", "darwin:1790770768:55074"} {
		t.Run(boot, func(t *testing.T) {
			store, td, _ := newAppTestTask(t, false)
			defer closeAppTestTask(t, store, td)
			recordRecoverableLifetime(t, td, boot)
			if err := guardian.RecoverTaskLifetime(t.Context(), td); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("unknown boot recovery = %v", err)
			}
			if _, err := td.ReadLifetimeTerminationContext(t.Context()); !task.RecordAbsent(err) {
				t.Fatalf("unknown epoch wrote termination: %v", err)
			}
			if _, err := td.ReadLifetimeWorkerWaitContext(t.Context()); !task.RecordAbsent(err) {
				t.Fatalf("unknown epoch wrote completion: %v", err)
			}
		})
	}
}
