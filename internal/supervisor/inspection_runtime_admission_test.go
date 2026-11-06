package supervisor

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestValidateInspectionRunnerKeepsRecordedPath(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	runner := filepath.Join(t.TempDir(), "recorded-worker")
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/historical/helper", HelperSHA256: strings.Repeat("b", 64),
		WorkerExecutable: runner, WorkerSHA256: strings.Repeat("c", 64),
		Supervisor: generationBinding(store.Root, "historical operation data"),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = ValidateInspectionRunner(operation, runner); err != nil {
		t.Fatalf("recorded worker required executable contents: %v", err)
	}
	if err = ValidateInspectionRunner(operation, filepath.Join(t.TempDir(), "other-worker")); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("different worker accepted: %v", err)
	}
}
