package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestValidateInspectionRunnerRejectsDigestDrift(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	request.TaskID = strings.Repeat("e", 32)
	client, logPath := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	runner, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: runner, WorkerSHA256: digest,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = ValidateInspectionRunner(operation, runner); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("worker digest drift was accepted: %v", err)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 0 {
		t.Fatalf("worker digest check launched supervisor observation %d times: %s", count, invocations)
	}
}
