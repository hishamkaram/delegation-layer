package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type historicalQueuedRunner struct {
	request    task.TaskRecord
	brief      []byte
	td         *taskdir.TaskDir
	meta       []byte
	before     map[string]string
	scheduling []byte
	history    []byte
}

func TestPublicStatusRejectsStagedCapabilityFailureBeforeQueuedRunnerRepair(t *testing.T) {
	t.Parallel()
	fixture := newHistoricalSupervisorFixture(t, false)
	queued := prepareHistoricalQueuedRunner(t, fixture)
	// Author the same installed finite CLI with an incomplete add help surface.
	// Real capability parsing must fail before the queued runner is touched.
	source := strings.Replace(historicalQueuedEditClientScript(fixture), "--stashed --print-task-id", "--print-task-id", 1)
	writeRunnerFixture(t, fixture.clientB, source)
	before := snapshotObservationPhaseRecords(t, fixture)
	queue := readHistoricalSupervisorBytes(t, fixture.statusPath)
	commands := observePhaseStatusCommands(fixture)
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--root", fixture.root, "status", fixture.req.TaskID, "--json"}, &stdout, &stderr, deps)
	response := decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
	requireFaultedStatusResponse(t, code, response, pueue.ErrStagedCapability.Error())
	if response.Error != pueue.ErrStagedCapability.Error() || response.Pending != nil || response.Supervisor != nil || response.Admission != task.AdmissionUnknown.String() {
		t.Errorf("capability failure lost its canonical cause or invented task observation: %+v", response)
	}
	assertObservationPhaseRecords(t, before)
	assertSupervisorProbeTestFile(t, fixture.statusPath, queue)
	assertSupervisorProbeTestFile(t, filepath.Join(queued.td.Dir, "meta.json"), queued.meta)
	assertSupervisorProbeTestFile(t, filepath.Join(fixture.root, ".supervisor", "migration-history.json"), queued.history)
	assertHistoricalTaskEvidence(t, fixture)
	if !maps.Equal(queued.before, historicalRepairReceipts(t, fixture)) {
		t.Error("failed capability probe published a runner repair receipt")
	}
	log := readHistoricalSupervisorBytes(t, fixture.commandLog)
	if bytes.Count(log, []byte("B parallel\n")) != 1 || bytes.Count(log, []byte("B add\n")) != 1 || bytes.Contains(log, []byte("B enqueue\n")) {
		t.Errorf("status did not reach exactly one scheduling attempt and failing add help probe: %q", log)
	}
	for _, argv := range commands() {
		// Parallelism precedes capability observation. No other mutation is
		// authorized by a failed staged capability probe.
		if slices.Contains(argv, "parallel") {
			continue
		}
		assertObservationPhaseNoMutation(t, [][]string{argv})
	}
}

// The additional row is genuinely submitted to the canonical task store under
// A. The finite CLI models Pueue's exact edit/readback protocol, never execution.
func prepareHistoricalQueuedRunner(t *testing.T, fixture *historicalSupervisorFixture) historicalQueuedRunner {
	t.Helper()
	brief := []byte("queued task admitted by predecessor A")
	request := newHistoricalSupervisorTaskRecord(fixture.store.RootID, fixture.req.CanonicalCwd, brief)
	request.TaskID = strings.Repeat("c", 32)
	oldRunner := filepath.Join(fixture.component.base, "old-installed-runner")
	writeRunnerFixture(t, oldRunner, "#!/bin/sh\nexit 0\n")
	profile := appTestExecutionProfile(&request, fixture.provider, nil)
	meta := tasklifecycle.NewMeta(request, profile, fixture.predecessor, oldRunner, task.RunnerOwnershipManaged, "historical-queued-runner")
	td, err := fixture.store.CreateTask(request.TaskID, &request, brief, &meta)
	fixture.component.ownTask(td)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := td.PrepareSubmission(fixture.predecessor)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := historicalConsumeSubmissionPermit(permit)
	if err != nil {
		t.Fatal(err)
	}
	id := int64(8)
	observation := pueue.Observation{
		Matched: true, State: pueue.StateQueued, Binding: fixture.predecessor, NumericTaskID: id,
		Identity: pueue.Identity{RootID: request.RootID, TaskID: request.TaskID, SpecSHA256: submission.SpecSHA256, MetaSHA256: submission.MetaSHA256, NumericTaskID: &id},
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = td.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	oldCommand := fmt.Sprintf("%s --root %s %s", oldRunner, fixture.root, request.TaskID)
	newCommand := fmt.Sprintf("%s --root %s %s", fixture.runner, fixture.root, request.TaskID)
	installHistoricalQueuedEdit(t, fixture, request, submission.Label, oldCommand, newCommand)
	if err = writePrivateSchedulingFixture(fixture.root, fixture.successor, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
		t.Fatal(err)
	}
	scheduling := readHistoricalSupervisorBytes(t, privateOrdinarySchedulingTestPath(fixture.root))
	writeCapabilitylessActiveGenerationSchedulingReceipt(t, fixture.root)
	return historicalQueuedRunner{
		request: request, brief: brief, td: td,
		meta: readTaskFile(t, td.Dir, "meta.json"), before: historicalRepairReceipts(t, fixture),
		scheduling: scheduling, history: readHistoricalSupervisorBytes(t, filepath.Join(fixture.root, ".supervisor", "migration-history.json")),
	}
}

func installHistoricalQueuedEdit(t *testing.T, fixture *historicalSupervisorFixture, request task.TaskRecord, label, oldCommand, newCommand string) {
	t.Helper()
	var snapshot struct {
		Tasks  map[string]json.RawMessage `json:"tasks"`
		Groups map[string]json.RawMessage `json:"groups"`
	}
	if err := json.Unmarshal(readHistoricalSupervisorBytes(t, fixture.statusPath), &snapshot); err != nil {
		t.Fatal(err)
	}
	job := savedPrivateCancelJob{
		ID: 8, CreatedAt: savedPrivateCancelStatusTime, OriginalCommand: oldCommand, Command: oldCommand,
		Path: request.CanonicalCwd, Envs: map[string]string{"RETAINED": "queue-row"}, Group: "default", Dependencies: []int64{}, Priority: 3, Label: label,
		Status: savedPrivateCancelVariant{Queued: &savedPrivateCancelQueued{EnqueuedAt: savedPrivateCancelStatusTime}},
	}
	row, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["8"] = row
	before, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job.OriginalCommand, job.Command = newCommand, newCommand
	row, err = json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tasks["8"] = row
	after, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	base := fixture.component.base
	for name, data := range map[string][]byte{"queued-before.json": before, "queued-after.json": after, "queued-old-command": []byte(oldCommand), "queued-new-command": []byte(newCommand)} {
		writeAppTestFile(t, filepath.Join(base, name), data)
	}
	writeAppTestFile(t, fixture.statusPath, before)
	writeRunnerFixture(t, fixture.clientB, historicalQueuedEditClientScript(fixture))
}

func historicalQueuedEditClientScript(fixture *historicalSupervisorFixture) string {
	base := fixture.component.base
	branch := fmt.Sprintf(`  edit)
    [ "${DELEGATE_PUEUE_EDIT_TASK_ID-}" = 8 ] || exit 64
    printf '%%s' "$DELEGATE_PUEUE_EDIT_EXPECTED_COMMAND" | /usr/bin/cmp -s - '%s/queued-old-command'
    printf '%%s' "$DELEGATE_PUEUE_EDIT_REPLACEMENT_COMMAND" | /usr/bin/cmp -s - '%s/queued-new-command'
    /usr/bin/cmp -s '%s/queued-before.json' '%s'
    /bin/cp '%s/queued-after.json' '%s'
    ;;
  shutdown)`, base, base, base, fixture.statusPath, base, fixture.statusPath)
	source := historicalSupervisorClientScript("B", fixture.readyPath, filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid"), fixture.statusPath, fixture.commandLog)
	source = strings.Replace(source, "|add|enqueue)", "|add|enqueue|edit)", 1)
	source = strings.Replace(source, "  shutdown)", branch, 1)
	return source
}

func historicalRepairReceipts(t *testing.T, fixture *historicalSupervisorFixture) map[string]string {
	t.Helper()
	directory := filepath.Join(fixture.root, "runner-repair")
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		return map[string]string{}
	} else if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]string)
	for _, name := range runnerRepairReceipts(t, &rebootFixture{historicalSupervisorFixture: fixture}) {
		result[name] = string(readHistoricalSupervisorBytes(t, filepath.Join(directory, name)))
	}
	return result
}

func assertHistoricalQueuedRepair(t *testing.T, fixture *historicalSupervisorFixture, queued historicalQueuedRunner, stopped bool) {
	t.Helper()
	after := historicalRepairReceipts(t, fixture)
	assertHistoricalRunnerRepairReceipts(t, fixture, queued.before, after)
	commands := readHistoricalSupervisorBytes(t, fixture.commandLog)
	for _, command := range []string{"edit", "parallel"} {
		if bytes.Count(commands, []byte("B "+command+"\n")) != 1 {
			t.Fatalf("completed successor did not perform exactly one %s: %q", command, commands)
		}
	}
	assertHistoricalQueuedEvidence(t, fixture, queued)
	starts := readHistoricalSupervisorBytes(t, fixture.daemonStartLog)
	resetHistoricalSupervisorCommandLog(t, fixture)
	if code, _, _ := runHistoricalStatusReconciliation(t, fixture); code != 0 {
		t.Fatalf("second completed-successor status exit=%d", code)
	}
	if !maps.Equal(after, historicalRepairReceipts(t, fixture)) {
		t.Fatal("second status published another repair receipt")
	}
	assertSupervisorProbeTestFile(t, fixture.daemonStartLog, starts)
	assertHistoricalQueuedEvidence(t, fixture, queued)
	assertHistoricalNoQueueMutation(t, fixture)
	if !stopped {
		assertHistoricalQueuedSameID(t, fixture, queued)
		assertSupervisorProbeTestFile(t, fixture.daemonStartLog, starts)
		assertHistoricalQueuedEvidence(t, fixture, queued)
		if !maps.Equal(after, historicalRepairReceipts(t, fixture)) {
			t.Fatal("same-ID retry published another repair receipt")
		}
	}
}

func assertHistoricalRunnerRepairReceipts(t *testing.T, fixture *historicalSupervisorFixture, before, after map[string]string) {
	t.Helper()
	if len(after) != len(before)+1 {
		t.Fatalf("repair receipt count=%d, want original %d plus one B receipt", len(after), len(before))
	}
	for name, original := range before {
		if data, found := after[name]; !found || original != data {
			t.Fatalf("historical repair receipt %q changed or disappeared", name)
		}
	}
	for name, data := range after {
		if original, existed := before[name]; existed {
			if original != data {
				t.Fatalf("historical repair receipt %q changed", name)
			}
			continue
		}
		var receipt struct {
			SchemaVersion      int                     `json:"schema_version"`
			Revision           string                  `json:"revision"`
			Supervisor         task.SupervisorRef      `json:"supervisor"`
			RunnerExecutable   string                  `json:"runner_executable"`
			RunnerSHA256       string                  `json:"runner_sha256,omitempty"`
			ExecutableIdentity task.ExecutableIdentity `json:"executable_identity,omitempty"`
		}
		if err := task.DecodeStrict([]byte(data), &receipt); err != nil || receipt.SchemaVersion != task.SchemaVersion || receipt.Revision != "managed-runner-v1" || receipt.Supervisor != fixture.successor || receipt.RunnerExecutable != fixture.runner || receipt.RunnerSHA256 != "" || receipt.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
			t.Fatalf("new repair receipt does not name repaired B and its installed runner: %+v err=%v", receipt, err)
		}
	}
}

func assertHistoricalQueuedEvidence(t *testing.T, fixture *historicalSupervisorFixture, queued historicalQueuedRunner) {
	t.Helper()
	assertSupervisorProbeTestFile(t, fixture.statusPath, readHistoricalSupervisorBytes(t, filepath.Join(fixture.component.base, "queued-after.json")))
	assertSupervisorProbeTestFile(t, filepath.Join(queued.td.Dir, "meta.json"), queued.meta)
	assertSupervisorProbeTestFile(t, privateOrdinarySchedulingTestPath(fixture.root), queued.scheduling)
	assertSupervisorProbeTestFile(t, filepath.Join(fixture.root, ".supervisor", "migration-history.json"), queued.history)
}

func assertHistoricalNoQueueMutation(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	commands := readHistoricalSupervisorBytes(t, fixture.commandLog)
	for _, command := range []string{"parallel", "edit", "add", "enqueue", "kill", "remove"} {
		if bytes.Contains(commands, []byte("B "+command+"\n")) {
			t.Fatalf("warm observation repeated queue mutation %s: %q", command, commands)
		}
	}
}

func assertHistoricalQueuedSameID(t *testing.T, fixture *historicalSupervisorFixture, queued historicalQueuedRunner) {
	t.Helper()
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	deps.Catalog = fixtureContinuationDependencies(t, nil, fixture.clientB).Catalog
	result := dispatch(Arguments{
		Root: fixture.root, TaskID: queued.request.TaskID, Provider: queued.request.Provider,
		Cwd: queued.request.CanonicalCwd, Config: queued.request.RequestedConfig, BriefBytes: queued.brief,
	}, deps)
	if result.code != 0 || result.err != nil || result.response.TaskID != queued.request.TaskID || !historicalSupervisorResponseMatches(result.response.Supervisor, 8) {
		t.Fatalf("same-ID dispatch lost the submitted A task through B: code=%d response=%+v err=%v", result.code, result.response, result.err)
	}
	assertSupervisorProbeTestFile(t, filepath.Join(queued.td.Dir, "meta.json"), queued.meta)
	assertHistoricalNoQueueMutation(t, fixture)
}
