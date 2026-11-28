package probe

import (
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
)

func TestUnlimitedInspectionRunsSelectedNativeRoutesAndJoinsCommands(t *testing.T) {
	for _, route := range probeBinderRoutes() {
		t.Run(route.name, func(t *testing.T) {
			temporary := binderTemporaryDirectory(t)
			definition := route.build(t, t.TempDir(), temporary)
			lifetime := &probeStartLifetime{t: t, definition: definition}
			workErr, stopErr := execution.RunSupervised(time.Time{}, execution.SupervisedOptions{Unlimited: true, Stopper: runtimeTestStopper{}, Lifetime: lifetime}, func(scope execution.PreflightScope) error {
				_, err := Inspect(scope, definition)
				return err
			})
			if workErr != nil || stopErr != nil {
				t.Fatalf("unlimited inspection: work=%v stop=%v", workErr, stopErr)
			}
			if got := lifetime.arguments(); !slices.EqualFunc(got, route.started, slices.Equal) {
				t.Fatalf("native starts=%q want=%q", got, route.started)
			}
			assertNoVerifiedViews(t, temporary)
		})
	}
}

func TestUnlimitedProjectionBoundDoesNotCancelTask(t *testing.T) {
	workErr, stopErr := execution.RunSupervised(time.Time{}, execution.SupervisedOptions{Unlimited: true, Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		before := time.Now()
		projected, cancelProjection := nativeProjectionContext(scope.Context())
		defer cancelProjection()
		deadline, bounded := projected.Deadline()
		if !bounded || deadline.Before(before.Add(contract.AdmissionTimeout)) || deadline.After(time.Now().Add(contract.AdmissionTimeout)) {
			t.Fatal("projection lost its finite auxiliary bound")
		}
		cancelProjection()
		if _, bounded := scope.Context().Deadline(); bounded {
			t.Fatal("projection imposed a task execution deadline")
		}
		return scope.Authorize()
	})
	if workErr != nil || stopErr != nil {
		t.Fatalf("projection affected parent: work=%v stop=%v", workErr, stopErr)
	}
}
