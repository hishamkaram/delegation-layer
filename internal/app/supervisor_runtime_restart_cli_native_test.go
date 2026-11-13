package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This test runs the public compiled CLI and runner. No fixture host, generation
// guard or Wait registry participates in the operation. Cleanup alone borrows
// the production read/control APIs for the creator's unique root.
func TestSupervisorRuntimeRestartPublicCLINative(t *testing.T) {
	clientPath, daemonPath := requireRuntimeRestartCLINativeContext(t)
	fixture := newRuntimeRestartCLINative(t, clientPath, daemonPath)
	fixture.build(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	fixture.ensureOwner(t, ctx)
	original := runtimeRestartCLIIncarnation{startup: fixture.preflight(t, ctx), daemon: fixture.executionTarget(t, ctx)}
	binding := fixture.binding(t)
	completed := fixture.restart(t, ctx, original.startup)
	successor := fixture.verifyRestart(t, ctx, original, completed)
	fixture.verifyReplay(t, ctx, original, successor)
	if after := fixture.binding(t); after != binding {
		t.Fatal("public runtime restart changed the full logical binding")
	}
	t.Log("actual CLI ensure-owner, native custody, exact restart/replay and original resident/daemon closure verified; no provider task launched")
}

type runtimeRestartCLIIncarnation struct {
	startup infrastructure.RuntimeRestartReference
	daemon  lifetime.ProcessRef
}

func (f *runtimeRestartCLINative) ensureOwner(t *testing.T, ctx context.Context) {
	t.Helper()
	f.started = true // Own partial initialization before its first effect.
	ready, code, err := f.command(ctx, "ensure-owner")
	if err != nil || code != 0 || ready.Status != "owner_ready" || task.ValidateRootID(ready.RootID) != nil {
		t.Fatalf("public ensure-owner: exit=%d status=%s err=%v", code, ready.Status, err)
	}
	f.rootID = ready.RootID
	f.pinRoot(t)
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatalf("original owner bookkeeping: %s", runtimeRestartCLIErrorClass(err))
	}
	err = errors.Join(f.ensureCleanupStartup(ctx, reader), reader.Close())
	if err != nil {
		t.Fatalf("original owner bookkeeping: %s", runtimeRestartCLIErrorClass(err))
	}
}

func (f *runtimeRestartCLINative) binding(t *testing.T) task.SupervisorRef {
	t.Helper()
	binding, found, err := pueue.ReadPrivateSupervisorBinding(f.root)
	if err != nil || !found || binding.ClientExecutable != f.originalClient || binding.DaemonExecutable != f.originalDaemon {
		t.Fatalf("public CLI did not select original in-place testkit: found=%t err=%v", found, err)
	}
	return binding
}

func (f *runtimeRestartCLINative) verifyRestart(t *testing.T, ctx context.Context, original runtimeRestartCLIIncarnation, completed SupervisorRuntimeRestartResponse) runtimeRestartCLIIncarnation {
	t.Helper()
	successor, err := infrastructure.ParseRuntimeRestartReference(completed.SuccessorStartup)
	if err != nil || successor.RootID != f.rootID || successor.Startup == original.startup.Startup {
		t.Fatalf("restart did not return an exact new startup: %v", err)
	}
	if current := f.preflight(t, ctx); current != successor {
		t.Fatal("ready startup differs from passive native custody observation")
	}
	newDaemon := f.executionTarget(t, ctx)
	same, err := lifetime.SameProcess(original.daemon, newDaemon)
	if err != nil || same {
		t.Fatalf("restart reused the predecessor process: same=%t err=%v", same, err)
	}
	f.assertOriginalClosed(t, ctx, original.startup, original.daemon)
	return runtimeRestartCLIIncarnation{startup: successor, daemon: newDaemon}
}

func (f *runtimeRestartCLINative) verifyReplay(t *testing.T, ctx context.Context, original, successor runtimeRestartCLIIncarnation) {
	t.Helper()
	// A completed reply may be lost. The same caller-known token must return
	// this receipt, never select the successor as another predecessor.
	replay := f.restart(t, ctx, original.startup)
	token, err := infrastructure.FormatRuntimeRestartReference(successor.startup)
	if err != nil || replay.SuccessorStartup != token || f.preflight(t, ctx) != successor.startup {
		t.Fatalf("identical restart selected a different successor: %v", err)
	}
	same, err := lifetime.SameProcess(successor.daemon, f.executionTarget(t, ctx))
	if err != nil || !same {
		t.Fatalf("completed replay launched another observed daemon: same=%t err=%v", same, err)
	}
}

func requireRuntimeRestartCLINativeContext(t *testing.T) (string, string) {
	t.Helper()
	client, daemon := os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED")
	if client == "" || daemon == "" {
		t.Skip("actual public CLI native test requires both original Pueue testkit paths")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("actual public CLI native restart requires a supported Unix host")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	source, err := infrastructure.CaptureInvocationSource(ctx)
	if err != nil {
		if errors.Is(err, lifetime.ErrTaskContained) {
			t.Skip("actual public CLI restart requires an outside invocation; current process is task-contained")
		}
		t.Fatalf("outside invocation could not be observed: %v", err)
	}
	if _, nested := source.Origin(); nested {
		t.Skip("actual public CLI restart requires an outside invocation; inherited origin is preserved")
	}
	return runtimeRestartCLIOriginalPair(t, client, daemon)
}

func runtimeRestartCLIOriginalPair(t *testing.T, client, daemon string) (string, string) {
	t.Helper()
	selected := []string{client, daemon}
	for index, path := range selected {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil || !filepath.IsAbs(resolved) {
			t.Fatalf("native testkit path unavailable: %v", resolveErr)
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("native testkit executable unavailable: %v", statErr)
		}
		selected[index] = resolved
	}
	if filepath.Dir(selected[0]) != filepath.Dir(selected[1]) || filepath.Base(selected[0]) != "pueue" || filepath.Base(selected[1]) != "pueued" {
		t.Fatal("native testkit must select its original adjacent pueue/pueued roles")
	}
	return selected[0], selected[1]
}

// The fixture owns its directory pins, built candidates and recorded subjects.
// An unresolved closure keeps the files; it never starts anything to clean up.
type runtimeRestartCLINative struct {
	base, root, bin, sourceRoot    string
	originalClient, originalDaemon string
	environment                    []string
	basePin, rootPin               *os.File
	started                        bool
	rootID                         string
	startups                       []infrastructure.StartupReference
	originalOwner                  lifetime.ProcessRef
}

func newRuntimeRestartCLINative(t *testing.T, client, daemon string) *runtimeRestartCLINative {
	t.Helper()
	fixture := &runtimeRestartCLINative{originalClient: client, originalDaemon: daemon}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 90*time.Second)
		defer cancel()
		cleanupErr := fixture.cleanup(ctx)
		if cleanupErr != nil {
			t.Errorf("retained public CLI fixture %s after unresolved cleanup: %v", fixture.base, cleanupErr)
		}
	})
	base, err := os.MkdirTemp("/tmp", "dlr-")
	if err != nil {
		t.Fatal(err)
	}
	fixture.base = base
	fixture.base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	fixture.basePin, err = os.Open(fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	fixture.root, fixture.bin = filepath.Join(fixture.base, "state"), filepath.Join(fixture.base, "bin")
	home := filepath.Join(fixture.base, "home")
	for _, path := range []string{fixture.bin, home} {
		if mkdirErr := os.Mkdir(path, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fixture.sourceRoot = filepath.Clean(filepath.Join(cwd, "..", ".."))
	fixture.environment = runtimeRestartCLIEnvironment(os.Environ(), map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"PATH": fixture.bin + string(os.PathListSeparator) + filepath.Dir(client) + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
	return fixture
}

func runtimeRestartCLIEnvironment(original []string, selected map[string]string) []string {
	result := make([]string, 0, len(original)+len(selected))
	for _, entry := range original {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := selected[key]; !replaced {
			result = append(result, entry)
		}
	}
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		result = append(result, key+"="+selected[key])
	}
	return result
}

func (f *runtimeRestartCLINative) build(t *testing.T) {
	t.Helper()
	owner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	f.requireBuildCompiler(t, owner, compiler)
	// Equivalent to the existing candidate-pair builder: only build source
	// into this owned installation. The canonical isolated owner joins the Go
	// compiler's entire tree; no installed executable is copied or hashed.
	for _, name := range []string{"delegate", "delegate-run"} {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
		command := exec.Command(compiler, "build", "-buildvcs=false", "-ldflags", "-X main.version=dev", "-o", filepath.Join(f.bin, name), "./cmd/"+name)
		command.Dir = f.sourceRoot
		command.Env = runtimeRestartCLIEnvironment(os.Environ(), map[string]string{"GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "GOPROXY": "off", "GOSUMDB": "off"})
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		buildErr := execution.RunIsolatedCommand(ctx, owner, command)
		cancel()
		if buildErr != nil {
			t.Fatalf("owned %s build failed after canonical compiler-tree cleanup: %v: %.4096s", name, buildErr, stdout.String()+stderr.String())
		}
	}
}

func (f *runtimeRestartCLINative) requireBuildCompiler(t *testing.T, owner, compiler string) {
	t.Helper()
	pin, err := os.ReadFile(filepath.Join(f.sourceRoot, ".go-version"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	command := exec.Command(compiler, "version")
	command.Dir = f.sourceRoot
	command.Env = runtimeRestartCLIEnvironment(os.Environ(), map[string]string{"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off"})
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err = execution.RunIsolatedCommand(ctx, owner, command); err != nil {
		t.Fatalf("pinned compiler observation failed: %v", err)
	}
	fields := strings.Fields(stdout.String())
	if len(fields) != 4 || fields[0] != "go" || fields[1] != "version" || fields[2] != "go"+strings.TrimSpace(string(pin)) || fields[3] != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatal("candidate builder requires the repository's pinned native Go compiler on PATH")
	}
}

// Actual CLI processes retain their outside context. The shared pipe/Wait
// owner joins each finite public command; it adds no native task containment.
type runtimeRestartCLICommandScope struct{ ctx context.Context }

func (s runtimeRestartCLICommandScope) Context() context.Context { return s.ctx }
func (s runtimeRestartCLICommandScope) StartCommand(_ *exec.Cmd, start func() error) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return start()
}
func (runtimeRestartCLICommandScope) FinishCommand(*exec.Cmd) error { return nil }

func (f *runtimeRestartCLINative) command(ctx context.Context, arguments ...string) (Response, int, error) {
	args := append([]string{"supervisor"}, arguments...)
	args = append(args, "--root", f.root, "--json")
	command := exec.Command(filepath.Join(f.bin, "delegate"), args...)
	command.Env, command.Dir = slices.Clone(f.environment), f.base
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := execution.RunScopedCommand(runtimeRestartCLICommandScope{ctx: ctx}, command, execution.CommandHooks{})
	code, known := runtimeRestartCLIExitCode(runErr)
	if !known {
		return Response{}, code, fmt.Errorf("public CLI completion unresolved: %w", runErr)
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return response, code, fmt.Errorf("public CLI response invalid: %w; stderr=%.2048s", err, stderr.String())
	}
	if response.SupervisorControl != nil && response.SupervisorControl.RuntimeRestart != nil {
		value := response.SupervisorControl.RuntimeRestart.SuccessorStartup
		if value != "" {
			if err := f.rememberStartup(value); err != nil {
				return response, code, err
			}
		}
	}
	if response.Error != "" && !runtimeRestartCLIPendingDiagnostic(response, code, arguments) {
		return response, code, fmt.Errorf("public CLI returned %s: %.2048s", response.Error, stderr.String())
	}
	return response, code, nil
}

// Exit/state/report identify pending work. Error is presentation only; check
// its fixture privacy bound without making its human wording a second policy.
func runtimeRestartCLIPendingDiagnostic(response Response, code int, arguments []string) bool {
	if code != 3 || len(response.Error) > 4096 || response.ErrorTruncated || strings.ContainsAny(response.Error, "/\\\r\n\x00") {
		return false
	}
	return runtimeRestartCLIPendingOperation(response, arguments)
}

func runtimeRestartCLIPendingOperation(response Response, arguments []string) bool {
	if len(arguments) != 3 || arguments[0] != "restart" || arguments[1] != "--from-startup" {
		return false
	}
	original, err := infrastructure.ParseRuntimeRestartReference(arguments[2])
	if err != nil || response.RootID != original.RootID || response.SchemaVersion != OutputSchemaVersion || response.Command != "supervisor" {
		return false
	}
	control := response.SupervisorControl
	if control == nil || control.RuntimeRestart == nil {
		return false
	}
	return control.Action == "restart" && control.State == "pending" && control.NextAction == "repeat_same_restart" &&
		control.RuntimeRestart.Phase != infrastructure.RuntimeRestartReady && validateRuntimeRestartCLIReport(original, *control.RuntimeRestart) == nil
}

func TestRuntimeRestartCLIPendingDiagnosticIsExact(t *testing.T) {
	original := infrastructure.RuntimeRestartReference{RootID: strings.Repeat("a", 32), Startup: infrastructure.StartupReference{
		Sequence: 1, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64),
	}}
	token, err := infrastructure.FormatRuntimeRestartReference(original)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{"restart", "--from-startup", token}
	for _, test := range []struct {
		name   string
		code   int
		mutate func(*Response)
		want   bool
	}{
		{name: "accepted pending diagnostic", code: 3, want: true},
		{name: "hard failure", code: 1},
		{name: "blocked", code: 3, mutate: func(response *Response) { response.SupervisorControl.State = "blocked" }},
		{name: "foreign root", code: 3, mutate: func(response *Response) { response.RootID = strings.Repeat("d", 32) }},
		{name: "unknown report", code: 3, mutate: func(response *Response) { response.SupervisorControl.RuntimeRestart.Phase = "unknown" }},
		{name: "foreign reference", code: 3, mutate: func(response *Response) { response.SupervisorControl.RuntimeRestart.FromStartup = "unknown" }},
		{name: "unsupported schema", code: 3, mutate: func(response *Response) { response.SchemaVersion++ }},
		{name: "private diagnostic", code: 3, mutate: func(response *Response) { response.Error = "/private/fixture: observation pending" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := Response{
				SchemaVersion: OutputSchemaVersion, Command: "supervisor", RootID: original.RootID, Error: "Observation remains pending.",
				SupervisorControl: &SupervisorControlResponse{
					Action: "restart", State: "pending", NextAction: "repeat_same_restart",
					RuntimeRestart: &SupervisorRuntimeRestartResponse{FromStartup: token, Phase: infrastructure.RuntimeRestartShutdownPending},
				},
			}
			if test.mutate != nil {
				test.mutate(&response)
			}
			if accepted := runtimeRestartCLIPendingDiagnostic(response, test.code, arguments); accepted != test.want {
				t.Fatalf("pending CLI diagnostic acceptance = %t; want %t", accepted, test.want)
			}
		})
	}
}

func runtimeRestartCLIExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	code := -1
	valid := task.AllErrorCauses(err, func(cause error) bool {
		var exit *exec.ExitError
		if !errors.As(cause, &exit) || !exit.Exited() || code >= 0 && exit.ExitCode() != code {
			return false
		}
		code = exit.ExitCode()
		return true
	})
	return code, valid && code >= 0
}

func (f *runtimeRestartCLINative) pinRoot(t *testing.T) {
	t.Helper()
	var err error
	f.rootPin, err = os.Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *runtimeRestartCLINative) rememberStartup(token string) error {
	reference, err := infrastructure.ParseRuntimeRestartReference(token)
	if err != nil || f.rootID != "" && reference.RootID != f.rootID {
		return errors.Join(infrastructure.ErrIdentity, err)
	}
	if !slices.Contains(f.startups, reference.Startup) {
		f.startups = append(f.startups, reference.Startup)
	}
	return nil
}

func (f *runtimeRestartCLINative) preflight(t *testing.T, ctx context.Context) infrastructure.RuntimeRestartReference {
	t.Helper()
	response, code, err := f.command(ctx, "status", "--runtime-restart")
	control := response.SupervisorControl
	if err != nil || code != 0 || response.RootID != f.rootID || control == nil || control.RuntimeRestart == nil {
		t.Fatalf("public passive restart custody: exit=%d facts=%+v", code, f.preflightFailureFacts(ctx, response, err))
	}
	observed := control.RuntimeRestart
	if observed.Custody != infrastructure.RuntimeRestartCustodyAvailable || observed.ObservedAt == "" || observed.OperationPending {
		t.Fatalf("current resident lacks exact native Wait custody: facts=%+v", f.preflightFailureFacts(ctx, response, nil))
	}
	if err = f.rememberStartup(observed.FromStartup); err != nil {
		t.Fatal(err)
	}
	reference, err := infrastructure.ParseRuntimeRestartReference(observed.FromStartup)
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

// Failure-only facts separate public transport from native target observation.
// Every field is an allowlisted enum/bool; paths, tokens and raw causes stay out.
func (f *runtimeRestartCLINative) preflightFailureFacts(ctx context.Context, response Response, cause error) runtimeRestartCLIPreflightFacts {
	facts := runtimeRestartCLIPreflightFacts{Platform: runtime.GOOS, PublicCause: runtimeRestartCLIErrorClass(cause), Custody: "missing", OwnerProbe: "reader_unavailable"}
	if control := response.SupervisorControl; control != nil && control.RuntimeRestart != nil {
		facts.Custody = "invalid"
		if custody := control.RuntimeRestart.Custody; custody.Validate() == nil {
			facts.Custody = string(custody)
		}
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err == nil {
		current, probeErr := infrastructure.ProbeOwner(ctx, reader)
		facts.KnownOwnerMatches = len(f.startups) > 0 && current == f.startups[len(f.startups)-1]
		facts.OwnerProbe = runtimeRestartCLIErrorClass(errors.Join(probeErr, reader.Close()))
	}
	target, targetErr := pueue.ObservePrivateExecutionTarget(ctx, f.root, pueue.Options{Environment: f.environment})
	facts.TargetCause = runtimeRestartCLIErrorClass(targetErr)
	switch target.State() {
	case pueue.ExecutionTargetUnknown:
		facts.Target = "unknown"
	case pueue.ExecutionTargetAbsent:
		facts.Target = "absent"
	case pueue.ExecutionTargetRunning:
		facts.Target = "running"
	}
	return facts
}

type runtimeRestartCLIPreflightFacts struct {
	Platform, PublicCause, Custody, OwnerProbe, Target, TargetCause string
	KnownOwnerMatches                                               bool
}

func runtimeRestartCLIErrorClass(err error) string {
	for _, match := range []struct {
		cause error
		name  string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{infrastructure.ErrOwnerIncompatible, "owner_incompatible"},
		{infrastructure.ErrIdentity, "identity"},
		{pueue.ErrExecutionTargetUnverified, "target_unverified"},
		{infrastructure.ErrOwnerRequest, "owner_request"},
		{task.ErrLockBusy, "lock_busy"},
	} {
		if errors.Is(err, match.cause) {
			return match.name
		}
	}
	if err != nil {
		return "unclassified"
	}
	return "ok"
}

func (f *runtimeRestartCLINative) executionTarget(t *testing.T, ctx context.Context) lifetime.ProcessRef {
	t.Helper()
	target, err := pueue.ObservePrivateExecutionTarget(ctx, f.root, pueue.Options{Environment: f.environment})
	if err != nil || target.State() != pueue.ExecutionTargetRunning {
		t.Fatalf("public execution target unverified: state=%v err=%v", target.State(), err)
	}
	reference, err := lifetime.NewProcessRef(target.PID(), target.StartToken())
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func (f *runtimeRestartCLINative) restart(t *testing.T, ctx context.Context, original infrastructure.RuntimeRestartReference) SupervisorRuntimeRestartResponse {
	t.Helper()
	token, err := infrastructure.FormatRuntimeRestartReference(original)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		response, code, commandErr := f.command(ctx, "restart", "--from-startup", token)
		control := response.SupervisorControl
		if commandErr != nil || response.RootID != f.rootID || control == nil || control.RuntimeRestart == nil || control.RuntimeRestart.FromStartup != token {
			t.Fatalf("public exact restart: exit=%d err=%v", code, commandErr)
		}
		if reportErr := validateRuntimeRestartCLIReport(original, *control.RuntimeRestart); reportErr != nil {
			t.Fatalf("public restart report invalid: %v", reportErr)
		}
		if code == 0 && control.State == "restarted" && control.RuntimeRestart.Phase == infrastructure.RuntimeRestartReady {
			return *control.RuntimeRestart
		}
		if code != 3 || control.State != "pending" || control.NextAction != "repeat_same_restart" {
			t.Fatalf("restart did not produce safe same-reference reconciliation: exit=%d state=%s next=%s", code, control.State, control.NextAction)
		}
	}
	t.Fatal("same-reference restart remained pending within its finite workflow")
	return SupervisorRuntimeRestartResponse{}
}

func validateRuntimeRestartCLIReport(original infrastructure.RuntimeRestartReference, view SupervisorRuntimeRestartResponse) error {
	from, err := infrastructure.ParseRuntimeRestartReference(view.FromStartup)
	if err != nil || from != original {
		return errors.Join(infrastructure.ErrIdentity, err)
	}
	report := infrastructure.RuntimeRestartReport{From: original.Startup, Phase: view.Phase}
	if view.SuccessorStartup != "" {
		successor, err := infrastructure.ParseRuntimeRestartReference(view.SuccessorStartup)
		if err != nil || successor.RootID != original.RootID {
			return errors.Join(infrastructure.ErrIdentity, err)
		}
		report.Successor = successor.Startup
	}
	return report.Validate()
}

func (f *runtimeRestartCLINative) assertOriginalClosed(t *testing.T, ctx context.Context, original infrastructure.RuntimeRestartReference, daemon lifetime.ProcessRef) {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	closed, observeErr := infrastructure.WaitOwnerProcessClosed(ctx, reader, original.Startup)
	closeErr := reader.Close()
	if observeErr != nil || closeErr != nil || closed.ProcessClosure != infrastructure.OwnerProcessClosed || closed.Startup != original.Startup {
		t.Fatalf("original resident closure unverified: %+v err=%v", closed, errors.Join(observeErr, closeErr))
	}
	if err = lifetime.WaitProcessAbsent(ctx, daemon); err != nil {
		t.Fatalf("original daemon closure unverified: %v", err)
	}
}

func (f *runtimeRestartCLINative) cleanup(ctx context.Context) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, f.closePins()) }()
	if f.started {
		if err := f.closeRuntime(ctx); err != nil {
			return err
		}
	}
	for _, held := range []struct {
		path string
		pin  *os.File
	}{{f.root, f.rootPin}, {f.base, f.basePin}} {
		if err := requireRuntimeRestartCLIDirectory(held.path, held.pin); err != nil {
			return err
		}
	}
	if err := f.closePins(); err != nil {
		return err // A descriptor cleanup fault cannot authorize path deletion.
	}
	if f.base == "" {
		return nil
	}
	if err := os.RemoveAll(f.base); err != nil {
		return err
	}
	if _, err := os.Lstat(f.base); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("owned CLI directory deletion unverified"), err)
	}
	return nil
}

func requireRuntimeRestartCLIDirectory(path string, pin *os.File) error {
	if pin == nil {
		return nil
	}
	info, err := pin.Stat()
	visible, statErr := os.Lstat(path)
	if err != nil || statErr != nil {
		return errors.Join(err, statErr)
	}
	if !visible.IsDir() || visible.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, visible) {
		return errors.New("owned CLI directory identity changed")
	}
	return nil
}

func (f *runtimeRestartCLINative) closePins() (resultErr error) {
	for _, pin := range []**os.File{&f.rootPin, &f.basePin} {
		if *pin != nil {
			resultErr = errors.Join(resultErr, (*pin).Close())
			*pin = nil
		}
	}
	return resultErr
}

func (f *runtimeRestartCLINative) closeRuntime(ctx context.Context) (resultErr error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if f.rootID != "" && reader.RootID() != f.rootID {
		return infrastructure.ErrIdentity
	}
	if err = f.ensureCleanupStartup(ctx, reader); err != nil {
		return err
	}

	if err = f.closeObservedDaemon(ctx); err != nil {
		return err
	}

	// No infrastructure RPC or resident wait occurs while bootstrap EX is held.
	current := f.startups[len(f.startups)-1]
	stopped, err := infrastructure.StopOwnerServingAt(ctx, reader, current)
	if err != nil || stopped.State != infrastructure.OwnerServingReleased {
		return errors.Join(errors.New("owned resident serving release unverified"), err)
	}
	for _, startup := range f.startups {
		closed, closeErr := infrastructure.WaitOwnerProcessClosed(ctx, reader, startup)
		if closeErr != nil || closed.ProcessClosure != infrastructure.OwnerProcessClosed {
			return errors.Join(errors.New("owned resident process closure unverified"), closeErr)
		}
		if startup == f.startups[0] {
			if err = f.verifyOriginalOwner(closed); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *runtimeRestartCLINative) ensureCleanupStartup(ctx context.Context, reader *taskdir.ReadOnlyStore) error {
	if reader == nil {
		return os.ErrInvalid
	}
	if f.rootID == "" || reader.RootID() != f.rootID || reader.Root() != f.root {
		return infrastructure.ErrIdentity
	}
	if err := requireRuntimeRestartCLIDirectory(f.root, f.rootPin); err != nil {
		return err
	}
	if len(f.startups) != 0 {
		return nil
	}
	observed, process, err := observeRuntimeRestartCLICleanupOwner(ctx, reader)
	if err != nil {
		return err
	}
	token, err := infrastructure.FormatRuntimeRestartReference(infrastructure.RuntimeRestartReference{RootID: f.rootID, Startup: observed})
	if err != nil {
		return err
	}
	if err = f.rememberStartup(token); err != nil {
		return err
	}
	f.originalOwner = process
	return nil
}

func observeRuntimeRestartCLICleanupOwner(ctx context.Context, reader *taskdir.ReadOnlyStore) (infrastructure.StartupReference, lifetime.ProcessRef, error) {
	observed, err := infrastructure.ProbeOwner(ctx, reader)
	if err != nil {
		return infrastructure.StartupReference{}, lifetime.ProcessRef{}, err
	}
	selected, err := infrastructure.ObserveOwnerProcessClosed(ctx, reader, observed)
	if err != nil || selected.ProcessClosure != infrastructure.OwnerProcessPending {
		return infrastructure.StartupReference{}, lifetime.ProcessRef{}, errors.Join(infrastructure.ErrIdentity, err)
	}
	process, err := selected.Process.Reference()
	if err != nil {
		return infrastructure.StartupReference{}, lifetime.ProcessRef{}, err
	}
	current, err := infrastructure.ProbeOwner(ctx, reader)
	if err != nil || current != observed {
		return infrastructure.StartupReference{}, lifetime.ProcessRef{}, errors.Join(infrastructure.ErrIdentity, err)
	}
	return observed, process, nil
}

func (f *runtimeRestartCLINative) verifyOriginalOwner(closed infrastructure.OwnerStopResult) error {
	process, err := closed.Process.Reference()
	if err != nil {
		return err
	}
	same, err := lifetime.SameProcess(process, f.originalOwner)
	if err != nil || !same {
		return errors.Join(infrastructure.ErrIdentity, err)
	}
	return nil
}

func TestRuntimeRestartCLICleanupBookkeepingSurvivesFailedPreflight(t *testing.T) {
	fixture := newRemoteResourceBoundary(t)
	ownRemoteResourceResident(t, fixture)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, fixture.store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	expected, err := infrastructure.ProbeOwner(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	// This existing component host refuses custody preflight. Its canonical
	// native owner and saved subject remain available for independent cleanup.
	if _, err = infrastructure.ReadRuntimeRestartPreflight(ctx, reader, infrastructure.StartupReference{}); !errors.Is(err, infrastructure.ErrOwnerRequest) {
		t.Fatalf("fixture did not refuse preflight: %v", err)
	}
	cleanup := &runtimeRestartCLINative{root: fixture.store.Root, rootID: fixture.store.RootID}
	if err = cleanup.ensureCleanupStartup(ctx, reader); err != nil {
		t.Fatalf("failed custody preflight prevented canonical cleanup bookkeeping: %v", err)
	}
	if !slices.Equal(cleanup.startups, []infrastructure.StartupReference{expected}) {
		t.Fatal("cleanup did not retain the exact authenticated original owner")
	}
	selected, err := infrastructure.ObserveOwnerProcessClosed(ctx, reader, expected)
	if err != nil || cleanup.verifyOriginalOwner(selected) != nil {
		t.Fatal("cleanup lost the exact original native owner process")
	}
	for _, rootID := range []string{"", "foreign-root"} {
		unowned := &runtimeRestartCLINative{root: fixture.store.Root, rootID: rootID}
		if err = unowned.ensureCleanupStartup(ctx, reader); !errors.Is(err, infrastructure.ErrIdentity) || len(unowned.startups) != 0 || unowned.originalOwner != (lifetime.ProcessRef{}) {
			t.Fatal("cleanup adopted a missing or foreign root identity")
		}
	}
	response := Response{Error: "private diagnostic", SupervisorControl: &SupervisorControlResponse{
		RuntimeRestart: &SupervisorRuntimeRestartResponse{Custody: infrastructure.RuntimeRestartCustodyUnknown, FromStartup: "private startup token"},
	}}
	facts := cleanup.preflightFailureFacts(ctx, response, errors.New("private error /secret/environment"))
	assertRuntimeRestartCLICleanupFailureFacts(t, facts)
}

func assertRuntimeRestartCLICleanupFailureFacts(t *testing.T, facts runtimeRestartCLIPreflightFacts) {
	t.Helper()
	if facts.PublicCause != "unclassified" || facts.Custody != "unknown" || facts.OwnerProbe != "ok" || !facts.KnownOwnerMatches || facts.Target != "absent" {
		t.Fatalf("failure facts lost their allowlisted native boundary: %+v", facts)
	}
	for _, secret := range []string{"private", "secret"} {
		if strings.Contains(fmt.Sprint(facts), secret) {
			t.Fatal("failure facts exposed a raw diagnostic or startup token")
		}
	}
}

func TestRuntimeRestartCLICleanupRefusesUnpublishedOwner(t *testing.T) {
	fixture := newRemoteResourceBoundary(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, fixture.store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	cleanup := &runtimeRestartCLINative{root: fixture.store.Root, rootID: fixture.store.RootID}
	err = cleanup.ensureCleanupStartup(ctx, reader)
	closeErr := reader.Close()
	if err == nil || closeErr != nil || len(cleanup.startups) != 0 || cleanup.originalOwner != (lifetime.ProcessRef{}) {
		t.Fatal("cleanup adopted a root without a canonical native startup")
	}
}

func (f *runtimeRestartCLINative) closeObservedDaemon(ctx context.Context) error {
	target, err := pueue.ObservePrivateExecutionTarget(ctx, f.root, pueue.Options{Environment: f.environment})
	if err != nil {
		return err // Pending/unknown birth is retained, never adopted for cleanup.
	}
	if target.State() == pueue.ExecutionTargetRunning {
		if err = f.shutdownDaemon(ctx, target); err != nil {
			return err
		}
	} else if target.State() != pueue.ExecutionTargetAbsent {
		return pueue.ErrExecutionTargetUnverified
	}
	return nil
}

func (f *runtimeRestartCLINative) shutdownDaemon(ctx context.Context, target pueue.ExecutionTarget) error {
	reference, err := lifetime.NewProcessRef(target.PID(), target.StartToken())
	if err != nil {
		return err
	}
	options := pueue.Options{Environment: f.environment, ControlOwnerExecutable: filepath.Join(f.bin, "delegate-run"), WaitForPendingClose: true}
	err = pueue.WithExistingBootstrapLockContext(ctx, f.root, func(locked context.Context) error {
		client, found, prepareErr := pueue.PreparePublishedPrivateWithLock(locked, f.root, options)
		if prepareErr != nil || !found || client == nil {
			if client != nil {
				prepareErr = errors.Join(prepareErr, client.CloseAfterPending(prepareErr))
			}
			return errors.Join(errors.New("owned published daemon client unavailable"), prepareErr)
		}
		shutdownErr := client.Shutdown(locked)
		if shutdownErr == nil {
			shutdownErr = client.WaitUnavailable(locked)
		}
		return errors.Join(shutdownErr, client.CloseAfterPending(shutdownErr))
	})
	if err != nil {
		return err
	}
	return lifetime.WaitProcessAbsent(ctx, reference)
}
