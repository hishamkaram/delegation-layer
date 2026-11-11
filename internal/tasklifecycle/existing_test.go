package tasklifecycle_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestDispatchExistingMissingPreparedRecordsCannotCreateWhenExistingOnly(t *testing.T) {
	store, td, request := newBriefFixture(t)
	// Model loss after the connector's complete-preparation prevalidation.
	if _, _, _, _, err := td.PreparedIdentity(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(td.Dir, "task.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(td.Dir, "meta.json")
	if err = os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	result := tasklifecycle.DispatchExistingContext(t.Context(), tasklifecycle.ExistingTaskOptions{ExistingOnly: true}, store, td, *request)
	if !errors.Is(result.Failure, os.ErrNotExist) || result.Stage != tasklifecycle.ExistingPreparation || result.Creation != nil || result.Submission != nil || result.Retried || result.TaskCloseError != nil {
		t.Fatalf("existing-only recovery attempted fresh work or lost preparation failure: %+v", result)
	}
	if _, err = os.Stat(meta); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing-only recovery repaired missing metadata: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing-only recovery changed canonical request: %v", err)
	}
}

func TestDispatchExistingDefaultKeepsIncompletePreparationFallback(t *testing.T) {
	store, td, request := newBriefFixture(t)
	if err := os.Remove(filepath.Join(td.Dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	result := tasklifecycle.DispatchExistingContext(t.Context(), tasklifecycle.ExistingTaskOptions{}, store, td, *request)
	// Empty creation dependencies stop before admission; the creation observation
	// proves ordinary callers still enter their established fallback owner.
	if result.Creation == nil || result.Submission != nil || result.Retried || result.TaskCloseError != nil {
		t.Fatalf("ordinary recovery lost its incomplete-preparation fallback: %+v", result)
	}
}
