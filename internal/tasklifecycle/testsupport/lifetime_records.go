package testsupport

import (
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func RecordRecoverableLifetime(t *testing.T, td *taskdir.TaskDir, bootID string) {
	t.Helper()
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	deadline := time.Now().Add(time.Minute)
	_, err = td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{
		PID:          1 << 30,
		ProcessStart: "test-worker-start",
		BootID:       bootID,
	})
	if err != nil {
		t.Fatal(err)
	}
}
