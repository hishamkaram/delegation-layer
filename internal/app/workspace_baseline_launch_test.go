package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// A managed task whose prelaunch baseline cannot be settled is refused at the
// preflight boundary: the provider never starts and a replay cannot launch.
func TestManagedTurnBaselineFailureRefusesProviderLaunch(t *testing.T) {
	store, original, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, original)
	td, req := createUnresolvableWorkspaceTask(t, store, original)
	profile := fixtureLaunchProfile(t, td, req)
	var starts atomic.Int32
	deps := Dependencies{
		Environment:    os.Environ(),
		PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) { return profile, nil },
		ExecutionHooks: execution.Hooks{Start: func(command *exec.Cmd) error {
			starts.Add(1)
			return command.Start()
		}},
	}
	result := runProvider(newResponse("dispatch"), store, td, req, profile, nil, nil, deps)
	if starts.Load() != 0 {
		t.Fatalf("provider started %d times without a settled baseline", starts.Load())
	}
	if result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
		t.Fatalf("refused baseline did not seal a rejected start: %+v", result.response)
	}
	replay := runProvider(newResponse("dispatch"), store, td, req, profile, nil, nil, deps)
	if replay.code == 0 || starts.Load() != 0 {
		t.Fatalf("replay after a refused baseline launched or succeeded: code=%d starts=%d", replay.code, starts.Load())
	}
}

// W3a is exercised through runProvider, rather than by calling the baseline
// writer: removing its production preflight wiring must refuse this Start.
// The journal precedes the pin and a later capture re-pins, so the pin is
// proved at Start from the shim's observation inside the task's own launch.
func TestManagedTurnBaselinePrecedesProviderStart(t *testing.T) {
	fixture := newNativeBaselineTask(t)
	writeLineageFile(t, filepath.Join(fixture.registration.CheckoutPath, "lead.txt"), "Lead before launch\n")
	scratch := fixture.baselineTemp(t, fixture.lineage.root)
	shim := installBaselineGitShim(t, fixture.lineage.root, fixture.baselineRef())
	owner := fixture.guardian(t, time.Minute)
	var baseline workspace.TurnBaseline
	deps := fixture.dependencies()
	deps.ExecutionHooks.Start = func(command *exec.Cmd) error {
		fixture.starts.Add(1)
		var err error
		baseline, err = fixture.resource.TurnBaseline(fixture.lineage.scope.ctx, fixture.turn.reservation.Sequence)
		if err != nil {
			return fmt.Errorf("baseline must precede provider Start: %w", err)
		}
		if baseline.Subject != fixture.turn.subject || baseline.ObservedHEAD != fixture.registration.BaseOID {
			return fmt.Errorf("baseline has another task or HEAD: %+v", baseline)
		}
		if err = shim.pinned(baseline, "Lead before launch\n"); err != nil {
			return err
		}
		return command.Start()
	}
	result := fixture.run(owner.child, deps)
	if result.code != 0 || result.err != nil || result.response.Publication != task.PublicationCommitted.String() || fixture.starts.Load() != 1 {
		t.Fatalf("managed launch: code=%d err=%v starts=%d response=%+v", result.code, result.err, fixture.starts.Load(), result.response)
	}
	if err := owner.finish(t); err != nil {
		t.Fatalf("native guardian completion: %v", err)
	}
	assertBaselineWorkerExited(t, fixture.td)
	assertBaselineScratchEmpty(t, scratch)
	capture := fixture.lineage.captureTurn(t, fixture.resource, fixture.turn)
	assertTurn(t, capture.Turn, workspace.TurnChanged, workspace.TurnUnchanged, workspace.TurnUnchanged)
	if capture.Turn.BaselineRef != baseline.Ref || capture.Turn.StartTreeOID != baseline.TreeOID {
		t.Fatalf("capture borrowed another baseline: %+v", capture.Turn)
	}
	assertArtifactBytes(t, fixture.lineage, capture, workspace.TurnNameStatusName, "A\x00"+baselineOddPath+"\x00A\x00turn.txt\x00")
	assertArtifactBytes(t, fixture.lineage, capture, workspace.CaptureNameStatusName, "A\x00lead.txt\x00A\x00"+baselineOddPath+"\x00A\x00turn.txt\x00")
	if content := fixture.lineage.gitIn(t, fixture.lineage.main, "show", capture.TreeOID+":"+baselineOddPath); content != "brief" {
		t.Fatalf("cumulative snapshot lost odd-path file content: %q", content)
	}
}

func createUnresolvableWorkspaceTask(t *testing.T, store *taskdir.Store, original *taskdir.TaskDir) (*taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	request, metadata, err := original.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	clonedRequest, clonedMetadata := *request, *metadata
	clonedRequest.TaskID = strings.Repeat("d", 32)
	clonedMetadata.TaskID = clonedRequest.TaskID
	clonedRequest.Workspace = &task.WorkspaceBinding{
		ID: strings.Repeat("e", 32), Reservation: 1,
		RequestSHA256: task.ComputeSHA256([]byte("missing workspace")), BudgetNanos: int64(time.Minute),
	}
	td, err := store.CreateTask(clonedRequest.TaskID, &clonedRequest, []byte("brief"), &clonedMetadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return td, &clonedRequest
}

func fixtureLaunchProfile(t *testing.T, td *taskdir.TaskDir, req *task.TaskRecord) PreparedProfile {
	t.Helper()
	metaPath := filepath.Join(td.Dir, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta task.MetaRecord
	if err = json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	providerExecutable, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	meta.ProviderExecutable = providerExecutable
	if metaData, err = task.MarshalCanonical(&meta); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(metaPath, metaData, 0o600); err != nil {
		t.Fatal(err)
	}
	providerBytes, err := os.ReadFile(providerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	return PreparedProfile{
		Plan:            execution.Plan{Executable: providerExecutable, ExecutableSHA256: task.ComputeSHA256(providerBytes), Directory: req.CanonicalCwd, Predicate: task.FixturePredicateRef()},
		ObservedVersion: "fixture-v2",
		Effective:       task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("app-test"))},
	}
}
