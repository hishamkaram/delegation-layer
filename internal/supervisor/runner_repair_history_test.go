package supervisor

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRunnerRepairHistoryUsesCanonicalReceiptsWithoutExecutableReads(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	receipt := runnerRepairReceipt{
		SchemaVersion: task.SchemaVersion, Revision: runnerRepairReceiptRevision,
		Supervisor:         generationBinding(fixture.store.Root, "historical receipt data"),
		RunnerExecutable:   filepath.Join(t.TempDir(), "absent-old-installation", "delegate-run"),
		ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
	}
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	name := "r-" + task.ComputeSHA256(data)[:48] + ".json"
	if err = publishRunnerRepairReceipt(t.Context(), fixture.store, name, receipt); err != nil {
		t.Fatal(err)
	}
	paths, err := recordedRepairRunnerPaths(t.Context(), fixture.store)
	if err != nil || !slices.Equal(paths, []string{receipt.RunnerExecutable}) {
		t.Fatalf("completed installed selection paths=%v err=%v", paths, err)
	}
	// A canonical record under another name is not a completed repair receipt.
	if err = publishRunnerRepairReceipt(t.Context(), fixture.store, "r-invalid.json", receipt); err != nil {
		t.Fatal(err)
	}
	if _, err = recordedRepairRunnerPaths(t.Context(), fixture.store); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("misnamed receipt authorized another command: %v", err)
	}
}
