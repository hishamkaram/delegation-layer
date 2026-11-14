package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type connectorCwdIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func connectorCwdStat(path string) (identity connectorCwdIdentity, resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return identity, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	var held unix.Stat_t
	if err = unix.Fstat(int(directory.Fd()), &held); err != nil {
		return identity, err
	}
	if held.Mode&unix.S_IFMT != unix.S_IFDIR {
		return identity, errors.New("cwd probe did not open a directory")
	}
	return connectorCwdIdentity{Device: uint64(held.Dev), Inode: uint64(held.Ino)}, nil
}

// The controlled provider's first filesystem operation observes its actual cwd.
// It writes outside either project, drains stdin and exits without descendants.
func TestConnectorCwdHelper(t *testing.T) {
	evidence := os.Getenv("DELEGATE_CONNECTOR_CWD_EVIDENCE")
	if evidence == "" {
		return
	}
	identity, err := connectorCwdStat(".")
	if err == nil {
		var data []byte
		data, err = json.Marshal(identity)
		if err == nil {
			err = os.WriteFile(evidence, data, 0o600)
		}
	}
	if err == nil {
		_, err = io.Copy(os.Stdout, os.Stdin)
	}
	if err != nil {
		os.Exit(31)
	}
	os.Exit(0)
}

func connectorCwdTask(t *testing.T, guarded bool) (*taskdir.TaskDir, *taskdir.StartPermit, Plan, string) {
	t.Helper()
	return connectorCwdTaskBinding(t, guarded, false)
}

func connectorCwdTaskBinding(t *testing.T, guarded, pathBinding bool) (*taskdir.TaskDir, *taskdir.StartPermit, Plan, string) {
	t.Helper()
	directory, plan, evidence := connectorCwdPreparedTaskBinding(t, guarded, pathBinding)
	permit, err := directory.PrepareStart(0)
	require(t, err)
	t.Cleanup(func() { require(t, permit.Release()) })
	return directory, permit, plan, evidence
}

func connectorCwdPreparedTaskBinding(t *testing.T, guarded, pathBinding bool) (*taskdir.TaskDir, Plan, string) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	require(t, err)
	t.Cleanup(func() { require(t, store.Close()) })
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	cwd := filepath.Join(parent, "project")
	require(t, os.Mkdir(cwd, 0o700))
	executable, err := os.Executable()
	require(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require(t, err)
	id, err := randomid.NewTaskID()
	require(t, err)
	brief := []byte("literal cwd probe\n")
	config := task.TaskConfig{Permission: "read-only", Budget: "20s"}
	request := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, RequestedConfig: config, BudgetNanos: int64(20 * time.Second), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	if guarded {
		if pathBinding {
			connectorPathCwdBind(t, store, request)
		} else {
			connectorCwdBind(t, store, request)
		}
	}
	meta := &task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, RequestedConfig: config, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("execution-test"))}, Containment: "fixture-only", Approval: "never", ProviderExecutable: executable, ProviderVersion: "fixture-v1", PublisherBuild: "test-build", PublisherVersion: "test", Predicate: task.FixturePredicateRef(), SupervisorConfig: task.SupervisorRef{ConfigPath: "/fake/pueue.yml", ConfigDigest: task.ComputeSHA256([]byte("pueue")), Endpoint: "/fake/socket", ObservedVersion: "fixture-v1"}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	evidence := filepath.Join(t.TempDir(), "actual-cwd.json")
	plan := Plan{Executable: executable, Arguments: []string{"-test.run=^TestConnectorCwdHelper$"}, Directory: cwd, Environment: append(os.Environ(), "DELEGATE_CONNECTOR_CWD_EVIDENCE="+evidence), Predicate: task.FixturePredicateRef()}
	directory, err := store.CreateTaskContext(t.Context(), id, request, brief, meta)
	require(t, err)
	t.Cleanup(func() { require(t, directory.Close()) })
	return directory, plan, evidence
}

func connectorCwdBind(t *testing.T, store *taskdir.Store, request *task.TaskRecord) {
	t.Helper()
	require(t, store.EnableConnectorContext(t.Context(), true))
	root, err := store.RootTokenContext(t.Context())
	require(t, err)
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	require(t, err)
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	require(t, err)
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", inventory.CompletenessReceipt, root)
	require(t, err)
	project, err := taskdir.ObserveProjectContext(t.Context(), request.CanonicalCwd)
	require(t, err)
	request.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project}
	request.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: request.TaskID, PrincipalRef: "principal", GatewayRequestSHA256: task.ComputeSHA256([]byte("cwd-probe"))}
}

func TestConnectorExecutionDoesNotRebindProjectAfterPermit(t *testing.T) {
	for _, phase := range []string{"preflight", "native_start"} {
		t.Run(phase, func(t *testing.T) { assertConnectorCwdNotRebound(t, phase) })
	}
}

type connectorCwdReplacement struct {
	phase         string
	path          string
	err           error
	starts, waits atomic.Int32
}

func (r *connectorCwdReplacement) replace() error {
	if r.err = os.Rename(r.path, r.path+"-moved"); r.err != nil {
		return r.err
	}
	r.err = os.Mkdir(r.path, 0o700)
	return r.err
}

func (r *connectorCwdReplacement) preflight(PreflightScope) error {
	if r.phase == "preflight" {
		return r.replace()
	}
	return nil
}

func (r *connectorCwdReplacement) start(command *exec.Cmd) error {
	if r.phase == "native_start" {
		if err := r.replace(); err != nil {
			return err
		}
	}
	r.starts.Add(1)
	return command.Start()
}

func (r *connectorCwdReplacement) wait(command *exec.Cmd) error {
	r.waits.Add(1)
	return command.Wait()
}

func assertConnectorCwdNotRebound(t *testing.T, phase string) {
	t.Helper()
	directory, permit, plan, evidence := connectorCwdTask(t, true)
	original, err := connectorCwdStat(plan.Directory)
	require(t, err)
	swap := &connectorCwdReplacement{phase: phase, path: plan.Directory}
	result := Run(directory, permit, plan, Options{
		Lifetime:  &connectorProjectGateLifetime{runner: plan.Executable},
		Preflight: swap.preflight,
		Hooks:     Hooks{Start: swap.start, Wait: swap.wait},
	})
	require(t, swap.err)
	data, readErr := os.ReadFile(evidence)
	if errors.Is(readErr, os.ErrNotExist) {
		if swap.starts.Load() != swap.waits.Load() || swap.starts.Load() > 1 {
			t.Fatalf("refused provider wrapper was not joined: Start=%d Wait=%d", swap.starts.Load(), swap.waits.Load())
		}
		if result.Outcome != nil && result.Outcome.Verdict == task.VerdictCommitted {
			t.Fatal("replacement refusal published accepted output")
		}
		if result.Error == nil && (result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected) {
			t.Fatal("unlaunched guarded provider has no explicit refusal")
		}
		return
	}
	require(t, readErr)
	assertSuccess(t, result)
	if swap.starts.Load() != 1 || swap.waits.Load() != 1 {
		t.Fatalf("provider Start/Wait ownership = %d/%d, want 1/1", swap.starts.Load(), swap.waits.Load())
	}
	var observed connectorCwdIdentity
	require(t, json.Unmarshal(data, &observed))
	if observed != original {
		t.Fatalf("guarded provider executed in replacement project: actual=%+v, admitted=%+v", observed, original)
	}
}

// This fixture exercises the real child launch gate and execution's retained
// directory port. It is not an authenticated guardian or native scope proof.
type connectorProjectGateLifetime struct {
	runner         string
	fault          error
	projectStarts  atomic.Int32
	ordinaryStarts atomic.Int32
}

func (l *connectorProjectGateLifetime) StartCommand(_ *exec.Cmd, start func() error) error {
	l.ordinaryStarts.Add(1)
	return start()
}

func (l *connectorProjectGateLifetime) StartProjectCommand(command *exec.Cmd, directory *os.File, launch task.ProjectLaunchContext, start func() error) error {
	l.projectStarts.Add(1)
	gate, err := guardian.NewLaunchGateFactory(l.runner)(command, &guardian.ProjectDirectory{File: directory, Context: launch})
	if err != nil {
		return err
	}
	startErr := start()
	gateErr := gate.Started(startErr)
	if startErr != nil {
		return errors.Join(startErr, gateErr, gate.Abort())
	}
	// Successful native Start retains execution's one Wait even if the gate
	// release fails. Report that ancillary fault only after Wait.
	l.fault = errors.Join(l.fault, gateErr, gate.Release())
	return nil
}

func (l *connectorProjectGateLifetime) FinishCommand(context.Context, *exec.Cmd) error {
	return l.fault
}
func (*connectorProjectGateLifetime) Terminate(context.Context) error   { return nil }
func (l *connectorProjectGateLifetime) CloseTask(context.Context) error { return l.fault }

func TestConnectorExecutionUnchangedProjectSealsAfterWorkCancellation(t *testing.T) {
	directory, permit, plan, evidence := connectorCwdTask(t, true)
	want, err := connectorCwdStat(plan.Directory)
	require(t, err)
	lifetime := &connectorProjectGateLifetime{runner: plan.Executable}
	assertSuccess(t, Run(directory, permit, plan, Options{Lifetime: lifetime}))
	data, err := os.ReadFile(evidence)
	require(t, err)
	var got connectorCwdIdentity
	require(t, json.Unmarshal(data, &got))
	if got != want {
		t.Fatalf("guarded provider cwd = %+v, want %+v", got, want)
	}
}

func TestConnectorExecutionRefusesUnsupportedLifetimeBeforePreflight(t *testing.T) {
	for _, lifetime := range []Lifetime{nil, recordingLifetime{events: &eventLog{}}} {
		directory, permit, plan, evidence := connectorCwdTask(t, true)
		preflights, starts := 0, 0
		result := Run(directory, permit, plan, Options{
			Lifetime:  lifetime,
			Preflight: func(PreflightScope) error { preflights++; return nil },
			Hooks:     Hooks{Start: func(*exec.Cmd) error { starts++; return errors.New("unexpected launch") }},
		})
		if !errors.Is(result.Error, task.ErrInvalidPermit) || starts != 0 || preflights != 0 || result.Outcome != nil {
			t.Fatalf("unsupported lifetime reached guarded work: starts=%d preflights=%d result=%+v", starts, preflights, result)
		}
		if _, err := os.Stat(evidence); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unsupported lifetime reached provider:", err)
		}
	}
}

type connectorProjectRestoreObserver struct {
	permit   *taskdir.StartPermit
	path     string
	observed error
}

func (*connectorProjectRestoreObserver) Observe([]byte) {}
func (o *connectorProjectRestoreObserver) Complete() error {
	if err := os.Rename(o.path, o.path+"-moved"); err != nil {
		return err
	}
	if err := os.Mkdir(o.path, 0o700); err != nil {
		return err
	}
	o.observed = o.permit.RunnerLease().ValidateProject(context.Background())
	return errors.Join(o.observed, os.Remove(o.path), os.Rename(o.path+"-moved", o.path))
}

func TestConnectorObservedMismatchCannotSealAfterProjectRestoration(t *testing.T) {
	directory, permit, plan, _ := connectorCwdTask(t, true)
	observer := &connectorProjectRestoreObserver{permit: permit, path: plan.Directory}
	result := Run(directory, permit, plan, Options{Lifetime: &connectorProjectGateLifetime{runner: plan.Executable}, Identity: observer})
	if !errors.Is(observer.observed, task.ErrIdentityMismatch) || !errors.Is(result.Error, task.ErrIdentityMismatch) || result.Outcome != nil {
		t.Fatalf("restoration cleared observed project mismatch: observed=%v result=%+v", observer.observed, result)
	}
	if _, err := os.Stat(plan.Directory + "-moved"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("original project was not restored:", err)
	}
}

func TestConnectorPreflightUsesProjectPortOnlyForMatchingCwd(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(map[bool]string{true: "project", false: "maintenance"}[matching], func(t *testing.T) {
			assertConnectorPreflightProjectPort(t, matching)
		})
	}
}

func assertConnectorPreflightProjectPort(t *testing.T, matching bool) {
	t.Helper()
	directory, permit, plan, _ := connectorCwdTask(t, true)
	preflightCwd := plan.Directory
	wantProject, wantOrdinary := int32(2), int32(0)
	if !matching {
		var err error
		preflightCwd, err = filepath.EvalSymlinks(t.TempDir())
		require(t, err)
		wantProject, wantOrdinary = 1, 1
	}
	want, err := connectorCwdStat(preflightCwd)
	require(t, err)
	marker := filepath.Join(t.TempDir(), "preflight.json")
	lifetime := &connectorProjectGateLifetime{runner: plan.Executable}
	result := Run(directory, permit, plan, Options{Lifetime: lifetime, Preflight: func(scope PreflightScope) error {
		command := exec.Command(plan.Executable, "-test.run=^TestConnectorCwdHelper$")
		command.Dir = preflightCwd
		command.Env = append(append([]string{}, plan.Environment...), "DELEGATE_CONNECTOR_CWD_EVIDENCE="+marker)
		return RunScopedCommand(scope, command, CommandHooks{})
	}})
	assertSuccess(t, result)
	if lifetime.projectStarts.Load() != wantProject || lifetime.ordinaryStarts.Load() != wantOrdinary {
		t.Fatalf("preflight lifetime ports: project=%d ordinary=%d, want %d/%d", lifetime.projectStarts.Load(), lifetime.ordinaryStarts.Load(), wantProject, wantOrdinary)
	}
	data, err := os.ReadFile(marker)
	require(t, err)
	var got connectorCwdIdentity
	require(t, json.Unmarshal(data, &got))
	if got != want {
		t.Fatalf("preflight cwd = %+v, want %+v", got, want)
	}
}

func TestConnectorChildGuardRefusalCannotAcceptAfterPathRestoration(t *testing.T) {
	directory, permit, plan, marker := connectorCwdTask(t, true)
	var starts, waits atomic.Int32
	var restoreErr error
	result := Run(directory, permit, plan, Options{Lifetime: &connectorProjectGateLifetime{runner: plan.Executable}, Hooks: Hooks{
		Start: func(command *exec.Cmd) error {
			if err := os.Rename(plan.Directory, plan.Directory+"-moved"); err != nil {
				return err
			}
			if err := os.Mkdir(plan.Directory, 0o700); err != nil {
				return err
			}
			starts.Add(1)
			return command.Start()
		},
		Wait: func(command *exec.Cmd) error {
			waitErr := command.Wait()
			waits.Add(1)
			restoreErr = errors.Join(os.Remove(plan.Directory), os.Rename(plan.Directory+"-moved", plan.Directory))
			return waitErr
		},
	}})
	require(t, restoreErr)
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if starts.Load() != 1 || waits.Load() != 1 || result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("child-only refusal was not retained after restoration: Start=%d Wait=%d result=%+v", starts.Load(), waits.Load(), result)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused child executed provider:", err)
	}
}

func TestConnectorCwdProbeObservesUnchangedProject(t *testing.T) {
	directory, permit, plan, evidence := connectorCwdTask(t, false)
	want, err := connectorCwdStat(plan.Directory)
	require(t, err)
	assertSuccess(t, Run(directory, permit, plan, Options{}))
	data, err := os.ReadFile(evidence)
	require(t, err)
	var got connectorCwdIdentity
	require(t, json.Unmarshal(data, &got))
	if got != want {
		t.Fatalf("controlled provider cwd = %+v, want %+v", got, want)
	}
}
