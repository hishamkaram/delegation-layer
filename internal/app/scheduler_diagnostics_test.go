package app

import (
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestSchedulerAdmissionPreservesConcreteFailureWithoutRawError(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	tasks := schedulerObservationFixture(store)
	definition := contract.Definition{DefinitionHeader: contract.DefinitionHeader{ScheduleID: strings.Repeat("e", 32), CreatedAt: time.Now().UTC(), Cron: "* * * * *", Timezone: "UTC", Request: contract.Request{Provider: "not-a-real-provider", CanonicalCwd: t.TempDir(), Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}}}, Brief: []byte("bounded test")}
	state, err := tasks.Dispatch(schedulerruntime.DispatchRequest{TaskID: strings.Repeat("f", 32), Definition: definition})
	if err == nil || state.Attempt == nil || state.Attempt.Code == "admission_failed" || state.Attempt.Code == "" || state.Attempt.Stage == "" || state.Attempt.NextAction == "" {
		t.Fatalf("concrete admission cause lost: %+v %v", state, err)
	}
	if strings.Contains(state.Attempt.Message, "not-a-real-provider") {
		t.Fatal("raw request leaked into diagnostic")
	}
	if validation := contract.ValidateAttempt(*state.Attempt); validation != nil {
		t.Fatalf("invalid persisted diagnostic: %v", validation)
	}
}

func TestSchedulerQuotaResetOutsideStorageDomainIsIgnored(t *testing.T) {
	for _, value := range []string{"9999-12-31T23:59:59Z", "0001-01-01T00:00:00Z", "bad"} {
		if got := schedulerQuotaReset(value); !got.IsZero() {
			t.Fatalf("unsafe quota advice %q: %v", value, got)
		}
	}
	value := "2030-01-01T00:00:00Z"
	if got := schedulerQuotaReset(value); got.IsZero() || got.Format(time.RFC3339Nano) != value {
		t.Fatalf("valid future reset lost: %v", got)
	}
}

func TestSchedulerPressureFailureDoesNotRecommendRetryingClaim(t *testing.T) {
	failure := schedulerAdmissionFailure(&tasklifecycle.ResourcePressureError{}, "not_created")
	if failure == nil || failure.Code != "resource_pressure" || failure.NextAction != "wait_next_occurrence" {
		t.Fatalf("scheduler recommends retrying claimed occurrence: %+v", failure)
	}
}
