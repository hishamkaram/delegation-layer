package testsupport

import (
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func RecordInspectionLifetimeRegistration(t *testing.T, operation *inspection.Operation, bootID, assurance string) {
	t.Helper()
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	_, err = operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: assurance,
		ScopePlan: "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: assurance,
		ScopeID:   "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{
		PID:          1 << 30,
		ProcessStart: "test-worker-start",
		BootID:       bootID,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func InspectionRecoveryBootID(t *testing.T) string {
	t.Helper()
	bootID, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	return bootID
}
