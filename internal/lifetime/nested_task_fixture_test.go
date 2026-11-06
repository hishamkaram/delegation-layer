package lifetime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// The external test package can call the task-store owner without creating a
// lifetime -> taskdir production dependency or reimplementing its lock policy.
func TestNestedTaskAbsenceFixture(t *testing.T) {
	mode := os.Getenv("DELEGATE_NESTED_FIXTURE_MODE")
	if mode != "task-absence" && mode != "task-identity" {
		t.Skip("entrypoint for canonical never-launched acceptance observation")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, os.Getenv("DELEGATE_NESTED_FIXTURE_ROOT"), app.NativePredicates())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	task, err := store.OpenTask(os.Getenv("DELEGATE_NESTED_FIXTURE_TASK"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := task.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	observation := struct {
		RootID       string `json:"root_id"`
		TaskID       string `json:"task_id"`
		NeverStarted bool   `json:"never_started"`
		RefusalSHA   string `json:"refusal_sha256"`
		Prepared     bool   `json:"prepared"`
		SpecSHA      string `json:"spec_sha256"`
		MetaSHA      string `json:"meta_sha256"`
	}{RootID: store.RootID, TaskID: task.TaskID}
	if mode == "task-identity" {
		_, _, observation.SpecSHA, observation.MetaSHA, err = task.PreparedIdentity()
		observation.Prepared = err == nil
	} else {
		observation.NeverStarted, observation.RefusalSHA, err = tasklifecycle.ProveScheduledTaskUnlaunchedContext(ctx, task)
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(os.Stdout, "NESTED_TASK_OBSERVATION\t"+string(data)); err != nil {
		t.Fatal(err)
	}
}
