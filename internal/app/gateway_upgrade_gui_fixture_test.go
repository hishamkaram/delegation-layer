package app

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// Only launchd is doubled. Manager owns the actual private request, claim,
// Finish, natural-exit observation and cleanup; the child enters public RunContext.
type gatewayGUIFixture struct {
	manager      *gatewayservice.Manager
	dependencies Dependencies
	profile      *gatewayUpgradeNativeProfile
	mu           sync.Mutex
	jobs         map[string]*gatewayGUIInvocation
	invocations  []*gatewayGUIInvocation
	children     sync.WaitGroup
	fault        error
	childControl gatewayservice.GUIUpgradeControl
}

type gatewayGUIInvocation struct {
	request gatewayservice.GUIUpgradeRequest
	target  string
	running bool
	exit    int
	claimed bool
	result  bool
	joined  bool
	cleaned bool
	noStart bool
}

type gatewayGUIJob struct {
	label     string
	arguments []string
	path      string
}

func newGatewayGUIFixture(t *testing.T, parent context.Context, base string, dependencies Dependencies, profile *gatewayUpgradeNativeProfile) *gatewayGUIFixture {
	t.Helper()
	executable, err := verifiedexec.LocatePath("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	f := &gatewayGUIFixture{dependencies: dependencies, profile: profile, jobs: make(map[string]*gatewayGUIInvocation)}
	f.manager, err = gatewayservice.New(gatewayservice.Config{
		Platform: gatewayservice.MacOS, ProfileDirectory: profile.directory, Executable: profile.source.DelegateExecutable,
		Directory: filepath.Join(base, "gui-units"), ManagerExecutable: executable, UID: os.Getuid(),
		Path: verifiedexec.EnvironmentValue(dependencies.Gateway.Environment, "PATH"), Runner: f,
		RuntimeInactive: func(ctx context.Context) (bool, error) { return profile.RuntimeInactive(ctx, profile.directory) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Registered after Native cleanup hooks, so every GUI child joins first.
	t.Cleanup(func() { f.close(t, parent) })
	return f
}

func (f *gatewayGUIFixture) Probe(ctx context.Context, _ string, args []string) (int, error) {
	if len(args) != 2 || args[0] != "print" {
		return -1, errors.New("unexpected GUI probe")
	}
	if args[1] == fmt.Sprintf("gui/%d", os.Getuid()) {
		return 0, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, found := f.jobs[args[1]]; found {
		return 0, ctx.Err()
	}
	return 113, ctx.Err()
}

func (f *gatewayGUIFixture) Run(ctx context.Context, _ string, args []string) (gatewayservice.Result, error) {
	if len(args) == 3 && args[0] == "bootstrap" {
		return gatewayservice.Result{}, f.bootstrap(ctx, args[1], args[2])
	}
	if len(args) != 2 {
		return gatewayservice.Result{}, errors.New("unexpected GUI command")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	job, found := f.jobs[args[1]]
	if !found {
		return gatewayservice.Result{ExitCode: 113}, ctx.Err()
	}
	switch args[0] {
	case "print":
		if job.running {
			return gatewayservice.Result{Stdout: fmt.Appendf(nil, "state = running\npid = %d\n", os.Getpid())}, ctx.Err()
		}
		return gatewayservice.Result{Stdout: fmt.Appendf(nil, "state = not running\nlast exit code = %d\n", job.exit)}, ctx.Err()
	case "bootout":
		var err error
		job.joined, err = f.marker(job.request, "joined")
		if err != nil {
			return gatewayservice.Result{}, err
		}
		delete(f.jobs, args[1])
		return gatewayservice.Result{}, ctx.Err()
	default:
		return gatewayservice.Result{}, errors.New("unsupported GUI command")
	}
}

func (f *gatewayGUIFixture) bootstrap(ctx context.Context, domain, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	job, err := decodeGatewayGUIJob(data)
	if err != nil {
		return err
	}
	if len(job.arguments) < 2 {
		return errors.New("GUI plist has no fixed entry")
	}
	arguments, err := ParseArguments(job.arguments[1:])
	if err != nil || arguments.Gateway.Action != "upgrade" || arguments.Gateway.Preview || !arguments.Gateway.AcceptExecutionPolicyRemoval {
		return errors.Join(task.ErrRequestConflict, err)
	}
	request, err := gatewayservice.ReadGUIUpgradeRequest(arguments.Gateway.GUIUpgradeRequest, f.profile.directory)
	if err != nil || request.RuntimePATH != job.path || request.Executable != job.arguments[0] || request.Root != arguments.Root {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	invocation := &gatewayGUIInvocation{request: request, target: domain + "/" + job.label, running: true}
	f.mu.Lock()
	for _, previous := range f.invocations {
		if previous.request.Invocation == request.Invocation {
			f.mu.Unlock()
			return errors.New("same GUI invocation bootstrapped twice")
		}
	}
	f.jobs[invocation.target] = invocation
	f.invocations = append(f.invocations, invocation)
	f.children.Add(1)
	f.mu.Unlock()
	child, cancel := context.WithDeadline(context.WithoutCancel(ctx), request.Deadline)
	go func() { defer f.children.Done(); defer cancel(); f.runChild(child, invocation, job) }()
	return nil
}

func (f *gatewayGUIFixture) runChild(ctx context.Context, invocation *gatewayGUIInvocation, job gatewayGUIJob) {
	dependencies := f.dependencies
	input := *dependencies.Gateway
	input.Executable, input.Environment, input.GUIUpgrade = job.arguments[0], []string{"PATH=" + job.path}, f.manager
	if f.childControl != nil {
		input.GUIUpgrade = f.childControl
	}
	// App must capture the child itself and construct real NativeMaintenance.
	input.NativeMaintenance = nil
	dependencies.Gateway, dependencies.Environment = &input, input.Environment
	var output, diagnostic bytes.Buffer
	code := RunContext(ctx, job.arguments[1:], &output, &diagnostic, dependencies)
	claimed, claimErr := f.marker(invocation.request, "claim")
	result, resultErr := f.readResult(invocation.request, code)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fault = errors.Join(f.fault, claimErr, resultErr)
	invocation.claimed, invocation.result = claimed, result
	invocation.exit, invocation.running = code, false
}

func (f *gatewayGUIFixture) marker(request gatewayservice.GUIUpgradeRequest, name string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(request.Profile, ".gui-upgrade", request.Invocation, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var marker struct {
		Invocation string `json:"invocation"`
	}
	if err = json.Unmarshal(data, &marker); err != nil || marker.Invocation != request.Invocation {
		return false, errors.Join(task.ErrIdentityMismatch, err)
	}
	return true, nil
}

func (f *gatewayGUIFixture) readResult(request gatewayservice.GUIUpgradeRequest, code int) (bool, error) {
	data, err := os.ReadFile(filepath.Join(request.Profile, ".gui-upgrade", request.Invocation, "result.json"))
	if errors.Is(err, os.ErrNotExist) && code != 0 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var result gatewayservice.GUIUpgradeResult
	if err = json.Unmarshal(data, &result); err != nil || result.Invocation != request.Invocation || result.ExitCode != code {
		return false, errors.Join(task.ErrIdentityMismatch, err)
	}
	return true, nil
}

func (f *gatewayGUIFixture) assertCompleted(t *testing.T, ctx context.Context, arguments []string, dependencies Dependencies, response gatewaycommand.Response, operation string) {
	t.Helper()
	f.children.Wait()
	f.mu.Lock()
	invocations := make([]gatewayGUIInvocation, len(f.invocations))
	for index, invocation := range f.invocations {
		invocations[index] = *invocation
	}
	jobs, fault := len(f.jobs), f.fault
	f.mu.Unlock()
	if len(invocations) == 0 || jobs != 0 || fault != nil {
		t.Fatalf("GUI route bypassed custody or left a job: invocations=%d jobs=%d fault=%v", len(invocations), jobs, fault)
	}
	assertGatewayGUICustody(t, invocations, operation)
	// Completed replay is a public command, not another GUI child bootstrap.
	before := len(invocations)
	var output, diagnostic bytes.Buffer
	code := RunContext(ctx, arguments, &output, &diagnostic, dependencies)
	f.mu.Lock()
	after := len(f.invocations)
	f.mu.Unlock()
	var replay gatewaycommand.Response
	if err := json.Unmarshal(output.Bytes(), &replay); err != nil || code != 0 || replay.Upgrade == nil || replay.Upgrade.Supervisor != response.Upgrade.Supervisor || after != before {
		t.Fatalf("completed public GUI replay changed outcome or bootstrapped: code=%d err=%v", code, err)
	}
}

func assertGatewayGUICustody(t *testing.T, invocations []gatewayGUIInvocation, operation string) {
	t.Helper()
	for index, invocation := range invocations {
		if !invocation.claimed || !invocation.result || !invocation.joined || !invocation.cleaned || invocation.noStart || invocation.running {
			t.Fatal("GUI invocation skipped real claim, Finish or natural-exit join")
		}
		if index > 0 && (invocation.request.ExpectedOperation != operation || invocation.request.PATHSource != "prior_receipt") {
			t.Fatal("explicit GUI reconciliation lost exactA or retained PATH")
		}
	}
}

func (f *gatewayGUIFixture) observeFinishedInvocation() {
	f.mu.Lock()
	if len(f.invocations) == 0 {
		f.mu.Unlock()
		return
	}
	last := f.invocations[len(f.invocations)-1]
	f.mu.Unlock()
	cleaned, cleanErr := f.marker(last.request, "cleaned")
	noStart, proofErr := f.marker(last.request, "not_started")
	f.mu.Lock()
	defer f.mu.Unlock()
	last.cleaned, last.noStart = cleaned, noStart
	f.fault = errors.Join(f.fault, cleanErr, proofErr)
}

func (f *gatewayGUIFixture) close(t *testing.T, parent context.Context) {
	t.Helper()
	f.children.Wait()
	f.mu.Lock()
	var pending *gatewayGUIInvocation
	for _, job := range f.jobs {
		pending = job
	}
	f.mu.Unlock()
	if pending != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		_, err := f.manager.RunGUIUpgrade(ctx, pending.request)
		var noStart *gatewayservice.GUIUpgradeNotStartedError
		if err != nil && !errors.As(err, &noStart) {
			t.Errorf("GUI fixture retained unresolved transport: %v", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.jobs) != 0 || f.fault != nil {
		t.Errorf("GUI fixture did not join/clean: jobs=%d fault=%v", len(f.jobs), f.fault)
	}
}

func decodeGatewayGUIJob(data []byte) (gatewayGUIJob, error) {
	var job gatewayGUIJob
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return job, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err = decoder.DecodeElement(&key, &start); err != nil {
			return job, err
		}
		if err = decodeGatewayGUIValue(decoder, &job, key); err != nil {
			return job, err
		}
	}
	if job.label == "" || len(job.arguments) < 2 || job.path == "" {
		return job, errors.New("incomplete GUI job plist")
	}
	return job, nil
}

func decodeGatewayGUIValue(decoder *xml.Decoder, job *gatewayGUIJob, key string) error {
	switch key {
	case "Label":
		return decoder.Decode(&job.label)
	case "ProgramArguments":
		var array struct {
			Strings []string `xml:"string"`
		}
		if err := decoder.Decode(&array); err != nil {
			return err
		}
		job.arguments = array.Strings
	case "EnvironmentVariables":
		var environment struct {
			Keys   []string `xml:"key"`
			Values []string `xml:"string"`
		}
		if err := decoder.Decode(&environment); err != nil {
			return err
		}
		if len(environment.Keys) != 1 || environment.Keys[0] != "PATH" || len(environment.Values) != 1 {
			return errors.New("GUI fixture received environment other than PATH")
		}
		job.path = environment.Values[0]
	default:
		var ignored struct {
			Value string `xml:",innerxml"`
		}
		return decoder.Decode(&ignored)
	}
	return nil
}

type gatewayGUIClaimCounter struct {
	*gatewayservice.Manager
	calls atomic.Int32
}

func (c *gatewayGUIClaimCounter) ClaimGUIUpgrade(ctx context.Context, request gatewayservice.GUIUpgradeRequest) (*gatewayservice.GUIUpgradeClaim, error) {
	c.calls.Add(1)
	return c.Manager.ClaimGUIUpgrade(ctx, request)
}

type gatewayGUIUnexpectedNative struct{ calls atomic.Int32 }

func (n *gatewayGUIUnexpectedNative) Verify(context.Context, string, string) error {
	n.calls.Add(1)
	return task.ErrRequestConflict
}

func (n *gatewayGUIUnexpectedNative) Inspect(context.Context, string, string) (gatewaycommand.NativeTransition, error) {
	n.calls.Add(1)
	return gatewaycommand.NativeTransition{}, task.ErrRequestConflict
}

func (n *gatewayGUIUnexpectedNative) Ensure(context.Context, string, string, gatewaycommand.NativeTransition) (gatewaycommand.NativeTransition, error) {
	n.calls.Add(1)
	return gatewaycommand.NativeTransition{}, task.ErrRequestConflict
}

func TestGatewayUpgradeGUIChildRefusesOtherProfileControl(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := verifiedexec.LocatePath("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	profile := newGatewayUpgradeNativeProfile(t, ctx, &runtimeRestartCLINative{root: store.Root, base: base, bin: base})
	profile.selected = executable
	services := &gatewayUpgradeNativeServices{profile: profile.directory}
	native := &gatewayGUIUnexpectedNative{}
	dependencies := Dependencies{Gateway: &gatewaycommand.Dependencies{
		Client: profile, Services: services, Executable: executable, NativeInstallation: native, Environment: []string{"PATH=/usr/bin:/bin"},
	}}
	gui := newGatewayGUIFixture(t, ctx, base, dependencies, profile)
	dependencies.Gateway.GUIUpgrade = gui.manager
	gui.dependencies = dependencies
	if err = os.Mkdir(filepath.Join(base, "other-profile"), 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := gatewayservice.New(gatewayservice.Config{
		Platform: gatewayservice.MacOS, ProfileDirectory: filepath.Join(base, "other-profile"), Executable: executable,
		Directory: filepath.Join(base, "other-units"), ManagerExecutable: executable, UID: os.Getuid(), Path: "/usr/bin:/bin", Runner: gui,
		RuntimeInactive: func(context.Context) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	control := &gatewayGUIClaimCounter{Manager: other}
	gui.childControl = control
	var output, diagnostic bytes.Buffer
	code := RunContext(ctx, []string{"gateway", "upgrade", "--root", store.Root, "--profile", profile.directory, "--accept-execution-policy-removal", "--json"}, &output, &diagnostic, dependencies)
	var response gatewaycommand.Response
	if err = json.Unmarshal(output.Bytes(), &response); err != nil || code != 3 || response.Failure != "gateway_upgrade_not_started" {
		t.Fatalf("other-profile claim did not fail closed: code=%d failure=%s err=%v", code, response.Failure, err)
	}
	if control.calls.Load() != 1 || native.calls.Load() != 0 || len(services.calls) != 0 || profile.migrated || profile.updated {
		t.Fatal("child ignored injected claim scope or reached Native/SDK/service mutation")
	}
}
