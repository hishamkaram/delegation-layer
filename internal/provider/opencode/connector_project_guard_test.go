package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This finite agent only negotiates initialize. Its second read distinguishes
// execution's refused session frame (EOF) from an incorrectly transmitted one.
func TestACPProjectGuardHelper(t *testing.T) {
	marker := os.Getenv("DELEGATE_ACP_PROJECT_GUARD_MARKER")
	if marker == "" {
		return
	}
	input := bufio.NewReader(os.Stdin)
	first, err := input.ReadString('\n')
	if err != nil {
		os.Exit(31)
	}
	if _, err = io.WriteString(os.Stdout, acpInitializeResult(1, true)+"\n"); err != nil {
		os.Exit(32)
	}
	second, err := input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		os.Exit(33)
	}
	if err = os.WriteFile(marker, []byte(first+second), 0o600); err != nil {
		os.Exit(34)
	}
	os.Exit(0)
}

// The adapter is real; only the one controlled filesystem race is injected
// after it constructs session/new or session/resume and before core delivery.
type acpProjectSwapProtocol struct {
	*acpProtocol
	path   string
	method string
	queued []byte
}

func (p *acpProjectSwapProtocol) Receive(line []byte) (execution.InteractionUpdate, error) {
	update, err := p.acpProtocol.Receive(line)
	return p.swapBeforeDelivery(update, err)
}

func (p *acpProjectSwapProtocol) ReceiveRecord(record providerjson.Value) (execution.InteractionUpdate, error) {
	update, err := p.acpProtocol.ReceiveRecord(record)
	return p.swapBeforeDelivery(update, err)
}

func (p *acpProjectSwapProtocol) swapBeforeDelivery(update execution.InteractionUpdate, protocolErr error) (execution.InteractionUpdate, error) {
	for _, frame := range update.Outbound {
		var request struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(frame, &request); err != nil {
			return update, errors.Join(protocolErr, err)
		}
		if request.Method != p.method {
			continue
		}
		p.queued = append([]byte(nil), frame...)
		if err := os.Rename(p.path, p.path+"-moved"); err != nil {
			return update, errors.Join(protocolErr, err)
		}
		return update, errors.Join(protocolErr, os.Mkdir(p.path, 0o700))
	}
	return update, protocolErr
}

// This fixture isolates the execution writer boundary. It is deliberately not
// an authenticated guardian, native containment or installed OpenCode proof.
type acpProjectWriterLifetime struct{}

func (acpProjectWriterLifetime) StartCommand(_ *exec.Cmd, start func() error) error {
	return start()
}

func (acpProjectWriterLifetime) StartProjectCommand(_ *exec.Cmd, _ *os.File, _ task.DirectoryIdentity, start func() error) error {
	return start()
}
func (acpProjectWriterLifetime) FinishCommand(context.Context, *exec.Cmd) error { return nil }
func (acpProjectWriterLifetime) Terminate(context.Context) error                { return nil }
func (acpProjectWriterLifetime) CloseTask(context.Context) error                { return nil }

func TestACPProjectReplacementRefusesNewAndResumeFrames(t *testing.T) {
	for _, method := range []string{"session/new", "session/resume"} {
		t.Run(method, func(t *testing.T) { assertACPProjectWriterRefusal(t, method) })
	}
}

func assertACPProjectWriterRefusal(t *testing.T, method string) {
	t.Helper()
	directory, permit, plan, request, marker := acpProjectGuardTask(t, method)
	expected := task.SessionExpectation{}
	if request.PriorSession != nil {
		expected = task.SessionExpectation{Required: true, ID: request.PriorSession.ConversationID}
	}
	protocol, err := newACPProtocol(*request, expected, directory.RecordProviderIdentity)
	acpProjectRequire(t, err)
	wrapped := &acpProjectSwapProtocol{acpProtocol: protocol, path: request.CanonicalCwd, method: method}
	var starts, waits atomic.Int32
	result := execution.Run(directory, permit, plan, execution.Options{Protocol: wrapped, Lifetime: acpProjectWriterLifetime{}, Hooks: execution.Hooks{
		Start: func(command *exec.Cmd) error { starts.Add(1); return command.Start() },
		Wait:  func(command *exec.Cmd) error { waits.Add(1); return command.Wait() },
	}})
	if starts.Load() != 1 || waits.Load() != 1 || !errors.Is(result.Error, task.ErrIdentityMismatch) || result.Outcome != nil {
		t.Fatalf("project mismatch did not refuse and join ACP: Start=%d Wait=%d result=%+v", starts.Load(), waits.Load(), result)
	}
	var queued struct {
		Method string `json:"method"`
		Params struct {
			Cwd       string `json:"cwd"`
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	acpProjectRequire(t, json.Unmarshal(wrapped.queued, &queued))
	if queued.Method != method || queued.Params.Cwd != request.CanonicalCwd {
		t.Fatalf("real adapter changed the absolute project frame: %+v", queued)
	}
	if request.PriorSession != nil && queued.Params.SessionID != request.PriorSession.ConversationID {
		t.Fatal("real adapter changed the exact continuation session")
	}
	actual, err := os.ReadFile(marker)
	acpProjectRequire(t, err)
	var transmitted struct {
		Method string `json:"method"`
	}
	if strings.Count(string(actual), "\n") != 1 {
		t.Fatalf("refused session frame reached agent stdin: %s", actual)
	}
	acpProjectRequire(t, json.Unmarshal(actual, &transmitted))
	if transmitted.Method != "initialize" {
		t.Fatalf("agent did not receive the real initialize frame: %s", actual)
	}
}

func acpProjectGuardTask(t *testing.T, method string) (*taskdir.TaskDir, *taskdir.StartPermit, execution.Plan, *task.TaskRecord, string) {
	t.Helper()
	registry, err := predicate.New(NewInterpreter(ModeReadOnly))
	acpProjectRequire(t, err)
	store, err := taskdir.InitStoreWithPredicates(filepath.Join(t.TempDir(), "state"), registry)
	acpProjectRequire(t, err)
	t.Cleanup(func() { acpProjectRequire(t, store.Close()) })
	parent, err := filepath.EvalSymlinks(t.TempDir())
	acpProjectRequire(t, err)
	cwd := filepath.Join(parent, "project")
	acpProjectRequire(t, os.Mkdir(cwd, 0o700))
	executable, err := os.Executable()
	acpProjectRequire(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	acpProjectRequire(t, err)
	id, err := randomid.NewTaskID()
	acpProjectRequire(t, err)
	brief := []byte(acpTestBrief)
	config := task.TaskConfig{Permission: ModeReadOnly, Budget: "20s"}
	request := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, Provider: Provider, Mode: ModeReadOnly, CanonicalCwd: cwd, RequestedConfig: config, BudgetNanos: int64(20 * time.Second), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	if method == "session/resume" {
		request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: testTaskID}
	}
	acpBindProject(t, store, request)
	meta := &task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, RequestedConfig: config, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("acp-project-guard"))}, Containment: "fixture-only", Approval: "never", ProviderExecutable: executable, ProviderVersion: "fixture", PublisherBuild: "test", PublisherVersion: "test", Predicate: ReferenceForMode(ModeReadOnly), SupervisorConfig: task.SupervisorRef{ConfigPath: "/fake/pueue.yml", ConfigDigest: task.ComputeSHA256([]byte("fixture")), Endpoint: "/fake/socket", ObservedVersion: "fixture"}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	marker := filepath.Join(t.TempDir(), "stdin.jsonl")
	plan := execution.Plan{Executable: executable, Arguments: []string{"-test.run=^TestACPProjectGuardHelper$"}, Directory: cwd, Environment: append(os.Environ(), "DELEGATE_ACP_PROJECT_GUARD_MARKER="+marker), Predicate: meta.Predicate}
	directory, err := store.CreateTaskContext(t.Context(), id, request, brief, meta)
	acpProjectRequire(t, err)
	t.Cleanup(func() { acpProjectRequire(t, directory.Close()) })
	permit, err := directory.PrepareStart(0)
	acpProjectRequire(t, err)
	t.Cleanup(func() { acpProjectRequire(t, permit.Release()) })
	return directory, permit, plan, request, marker
}

func acpBindProject(t *testing.T, store *taskdir.Store, request *task.TaskRecord) {
	t.Helper()
	acpProjectRequire(t, store.EnableConnectorContext(t.Context(), true))
	root, err := store.RootTokenContext(t.Context())
	acpProjectRequire(t, err)
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	acpProjectRequire(t, err)
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	acpProjectRequire(t, err)
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", inventory.CompletenessReceipt, root)
	acpProjectRequire(t, err)
	project, err := taskdir.ObserveProjectContext(t.Context(), request.CanonicalCwd)
	acpProjectRequire(t, err)
	request.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project}
	request.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: request.TaskID, PrincipalRef: "principal", GatewayRequestSHA256: task.ComputeSHA256([]byte("acp-project"))}
}

func acpProjectRequire(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
