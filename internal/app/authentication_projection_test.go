package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestSchedulerAuthenticationAttributionPrecedesOptionalUsage(t *testing.T) {
	outcome := &task.OutcomeRecord{Verdict: task.VerdictRejected, Diagnostics: []task.OutputDiagnostic{{Code: "optional_usage_unavailable", Message: "optional"}, task.ProviderAuthenticationFailureDiagnostic()}}
	if got := schedulerRejectionCode(outcome, nil); got != "provider_authentication_failed" {
		t.Fatalf("attribution=%s", got)
	}
	attempt := &contract.AttemptFailure{Code: "admission_failed"}
	if got := schedulerRejectionCode(outcome, attempt); got != attempt.Code {
		t.Fatalf("attempt priority lost: %s", got)
	}
	outcome.Diagnostics = append(outcome.Diagnostics, task.OutputDiagnostic{Code: "provider_quota_exhausted", Message: "quota"})
	if got := schedulerRejectionCode(outcome, attempt); got != "provider_quota_exhausted" {
		t.Fatalf("quota priority lost: %s", got)
	}
}

func TestRejectedRecoveryNativeLoginGuidance(t *testing.T) {
	available := false
	for _, test := range []struct {
		name        string
		diagnostics []task.OutputDiagnostic
		failure     *DispatchFailureResponse
		want        string
	}{
		{name: "validated", diagnostics: []task.OutputDiagnostic{task.ProviderAuthenticationFailureDiagnostic()}, want: "run_native_provider_login"},
		{name: "code alone", diagnostics: []task.OutputDiagnostic{{Code: "provider_authentication_failed", Message: "untrusted"}}, want: "stop_and_report"},
		{name: "operational", diagnostics: []task.OutputDiagnostic{task.ProviderAuthenticationFailureDiagnostic()}, failure: &DispatchFailureResponse{Code: "storage_failed", NextAction: "stop_and_report"}, want: "stop_and_report"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := newResponse("collect")
			response.Outcome = &task.OutcomeRecord{Verdict: task.VerdictRejected, Predicate: task.PredicateRef{Adapter: "claude:print"}, Diagnostics: test.diagnostics}
			response.Failure = test.failure
			applyRejectedRecovery(&response, &available)
			if response.Failure == nil || response.Failure.NextAction != test.want {
				t.Fatalf("failure=%+v want %s", response.Failure, test.want)
			}
			if test.name == "operational" && response.Failure.Code != "storage_failed" {
				t.Fatal("operational failure replaced")
			}
		})
	}
}

func TestAuthenticationGuidancePreservesCollectionFault(t *testing.T) {
	available := false
	fault := errors.New("storage unavailable")
	for _, release := range []bool{false, true} {
		completion := &tasklifecycle.CollectionCompletion{Outcome: &task.OutcomeRecord{Verdict: task.VerdictRejected, Predicate: task.PredicateRef{Adapter: "claude:print"}, Diagnostics: []task.OutputDiagnostic{task.ProviderAuthenticationFailureDiagnostic()}}, RecoveryAvailable: &available}
		if release {
			completion.ReleaseError = fault
		} else {
			completion.CollectionError = fault
		}
		result := completedCollectionResponse(newResponse("collect"), completion, "", fault)
		if result.code != 1 || !errors.Is(result.err, fault) || result.response.Failure != nil {
			t.Fatalf("operational error lost: %+v", result)
		}
	}
}

func TestSchedulerAuthenticationResultFlowsToCachedConsumers(t *testing.T) {
	_, store, definition := scheduleFixture(t)
	definition.ScheduleID = strings.Repeat("f", 32)
	definition.Hook = &contract.FailureHook{Executable: filepath.Join(t.TempDir(), "hook"), Threshold: 1, Timeout: time.Second}
	if err := store.CreateSchedule(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	first, _, last := seedScheduleHistory(t, store, definition)
	writer, ok := store.(contract.ReliabilityStore)
	if !ok {
		t.Fatal("store lacks result owner")
	}
	input := int64(7)
	outcome := &task.OutcomeRecord{Verdict: task.VerdictRejected, Diagnostics: []task.OutputDiagnostic{{Code: "optional_usage_unavailable", Message: "optional usage malformed"}, task.ProviderAuthenticationFailureDiagnostic()}, Usage: []task.UsageMetadata{{Scope: task.UsageScopeMainAgent, Source: task.UsageSourceProviderEnvelope, Reliability: task.UsageReliabilityUnreliable, InputTokens: &input}}}
	fact := schedulerResultFact(first, taskdir.TaskEvidence{Exists: true, OutcomeSHA256: strings.Repeat("a", 64), Inspection: &taskdir.TaskInspection{Outcome: outcome}}, tasklifecycle.PassiveTaskObservation{Publication: task.PublicationRejected})
	if fact.Code != "provider_authentication_failed" || fact.Verdict != "rejected" {
		t.Fatalf("result misattributed: %+v", fact)
	}
	if err := writer.RecordResult(t.Context(), fact); err != nil {
		t.Fatal(err)
	}
	later := fact
	later.TaskID = last.TaskID
	later.ObservedAt = fact.ObservedAt.Add(time.Second)
	if err := writer.RecordResult(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	requireCachedAuthenticationHistory(t, store, definition.ScheduleID, first.TaskID, fact.Code)
	health, err := writer.Health(t.Context(), definition.ScheduleID, fact.ObservedAt)
	if err != nil || health.DominantFailureCode != fact.Code {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	notification, err := writer.ClaimNotification(t.Context(), definition.ScheduleID, strings.Repeat("9", 32), fact.ObservedAt)
	if err != nil || notification == nil || notification.FailureCode != fact.Code {
		t.Fatalf("hook notification=%+v err=%v", notification, err)
	}
}

func requireCachedAuthenticationHistory(t *testing.T, store contract.Store, scheduleID, taskID, code string) {
	t.Helper()
	page, err := store.History(t.Context(), contract.HistoryPage{ScheduleID: scheduleID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range page.Entries {
		occurrence, ok := entry.Occurrence()
		if ok && occurrence.TaskID == taskID {
			response := scheduleOccurrence(occurrence)
			found = true
			if response.ResultCode != code || response.ResultStatus != "rejected" || response.ResultObservedAt == nil {
				t.Fatalf("history lost attribution: %+v", response)
			}
		}
	}
	if !found {
		t.Fatal("history lost result")
	}
}
