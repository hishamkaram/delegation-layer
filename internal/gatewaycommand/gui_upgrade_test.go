package gatewaycommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

func TestGUIUpgradeUncertaintyRetainsIndependentHardFaults(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  int
	}{
		{"unknown", gatewayservice.ErrGUIUpgradeUnknown, 3},
		{"observation canceled", errors.Join(gatewayservice.ErrGUIUpgradeUnknown, context.Canceled), 3},
		{"observation expired", errors.Join(gatewayservice.ErrGUIUpgradeUnknown, context.DeadlineExceeded), 3},
		{"manager fault", errors.Join(gatewayservice.ErrGUIUpgradeUnknown, gatewayservice.ErrManager), 1},
		{"record fault", errors.Join(gatewayservice.ErrGUIUpgradeUnknown, gatewayservice.ErrGUIUpgradeRecord), 1},
		{"write fault", errors.Join(gatewayservice.ErrGUIUpgradeUnknown, errors.New("record write failed")), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, code := publicError(test.cause)
			if code != test.want {
				t.Fatalf("mixed hard fault became pending: code=%d want=%d", code, test.want)
			}
			if !errors.Is(test.cause, gatewayservice.ErrGUIUpgradeUnknown) {
				t.Fatal("uncertainty cause was discarded")
			}
		})
	}
}

func TestGUIUpgradeChildPinsSavedSourceRootAndRuntimeBeforeQuiesce(t *testing.T) {
	f := newUpgradeFixture(t)
	f.dependencies.Environment = []string{"PATH=/validated/runtime/path"}
	call := invocation{root: f.root, profile: f.profile, dependencies: f.dependencies, arguments: Arguments{AcceptExecutionPolicyRemoval: true}}
	base := gatewayservice.GUIUpgradeRequest{Profile: f.profile, Root: f.root, ExpectedRoot: f.rootToken, SavedSource: gatewayservice.GUIUpgradeSource{Executable: f.oldExecutable, Root: f.root, Context: f.rootToken}, Executable: f.newExecutable, Accepted: true, RuntimePATH: "/validated/runtime/path"}
	if err := call.validateGUIChildRequest(boundedTestContext(t), base); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"source", "root", "token", "selected", "PATH"} {
		t.Run(kind, func(t *testing.T) {
			r := base
			switch kind {
			case "source":
				r.SavedSource.Executable += "-other"
			case "root":
				r.Root += "-other"
			case "token":
				r.ExpectedRoot += "-other"
			case "selected":
				r.Executable = f.oldExecutable
			case "PATH":
				r.RuntimePATH = "/ambient/GUI"
			}
			if err := call.validateGUIChildRequest(boundedTestContext(t), r); err == nil || len(f.services.calls) != 0 {
				t.Fatalf("changed private request accepted or quiesced connector: %v", err)
			}
		})
	}
}

func TestGUIUpgradeExpectedOperationNeverFallsBackToCurrentSupervisor(t *testing.T) {
	native, runner := upgradePublicFixture(t)
	native.expectedOperation = "upgrade-v1:" + strings.Repeat("a", 32)
	if _, err := native.Inspect(boundedTestContext(t), runner.root, runner.rootToken); err == nil || len(runner.calls) != 0 {
		t.Fatalf("missing exact operation fell back to current status: calls=%v err=%v", runner.calls, err)
	}
}

// Only launchd and the process boundary are doubled. Control-slot persistence,
// child claims/profile coordination and SDK migration/source CAS are real.
type guiSDKRunner struct {
	mu              sync.Mutex
	manager         *gatewayservice.Manager
	fixture         *upgradeFixture
	loaded, running bool
	exit            int
	starts          int
	requests        []gatewayservice.GUIUpgradeRequest
	wg              sync.WaitGroup
	closeErr        error
}

func (r *guiSDKRunner) Probe(ctx context.Context, _ string, args []string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Count(args[1], "/") == 1 {
		return 0, ctx.Err()
	}
	if r.loaded {
		return 0, ctx.Err()
	}
	return 113, ctx.Err()
}

func (r *guiSDKRunner) Run(ctx context.Context, _ string, args []string) (gatewayservice.Result, error) {
	if args[0] == "bootstrap" {
		request, err := gatewayservice.ReadGUIUpgradeRequest(filepath.Join(filepath.Dir(args[2]), "request.json"), r.fixture.profile)
		if err != nil {
			return gatewayservice.Result{}, err
		}
		r.mu.Lock()
		r.loaded, r.running = true, true
		r.starts++
		r.requests = append(r.requests, request)
		r.mu.Unlock()
		child, cancel := context.WithDeadline(context.WithoutCancel(ctx), request.Deadline)
		r.wg.Add(1)
		go func() { defer r.wg.Done(); defer cancel(); r.child(child, request) }()
		return gatewayservice.Result{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.loaded {
		return gatewayservice.Result{ExitCode: 113}, nil
	}
	if args[0] == "bootout" {
		r.loaded = false
		return gatewayservice.Result{}, nil
	}
	if r.running {
		return gatewayservice.Result{Stdout: fmt.Appendf(nil, "state = running\npid = %d\n", os.Getpid())}, nil
	}
	return gatewayservice.Result{Stdout: fmt.Appendf(nil, "state = not running\nlast exit code = %d\n", r.exit)}, nil
}

func (r *guiSDKRunner) child(ctx context.Context, request gatewayservice.GUIUpgradeRequest) {
	claim, err := r.manager.ClaimGUIUpgrade(ctx, request)
	code := 1
	if err == nil {
		deps := r.fixture.dependencies
		deps.Environment = []string{"PATH=" + request.RuntimePATH}
		call := invocation{root: request.Root, profile: request.Profile, dependencies: deps, arguments: Arguments{Action: "upgrade", AcceptExecutionPolicyRemoval: request.Accepted}}
		response := Response{SchemaVersion: 1, Command: "gateway", Action: "upgrade", Profile: request.Profile, Online: "unknown", ExecutionReadiness: "unknown"}
		_, result, operationErr := call.executeGUIUpgrade(ctx, request, response)
		// Nonzero public exit reports the expected business failure.
		if operationErr != nil && result.ExitCode == 0 {
			err = operationErr
		}
		err = errors.Join(err, claim.Finish(result))
		code = result.ExitCode
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeErr = errors.Join(r.closeErr, err)
	r.exit, r.running = code, false
}

func guiSDKControl(t *testing.T, f *upgradeFixture) *guiSDKRunner {
	t.Helper()
	if f.gui != nil {
		return f.gui
	}
	executable, err := verifiedexec.LocatePath("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &guiSDKRunner{fixture: f}
	r.manager, err = gatewayservice.New(gatewayservice.Config{Platform: gatewayservice.MacOS, ProfileDirectory: f.profile, Executable: f.oldExecutable, Directory: filepath.Join(base, "units"), ManagerExecutable: executable, UID: os.Getuid(), Path: "/validated/runtime/path", Runner: r, RuntimeInactive: func(ctx context.Context) (bool, error) { return f.client.RuntimeInactive(ctx, f.profile) }})
	if err != nil {
		t.Fatal(err)
	}
	f.dependencies.GUIUpgrade = r.manager
	f.dependencies.NativeMaintenance = &upgradeMaintenanceReply{}
	f.gui = r
	t.Cleanup(func() {
		r.wg.Wait()
		if r.closeErr != nil {
			t.Error(r.closeErr)
		}
	})
	return r
}

func TestGUIUpgradeInitialVerificationFailureAllowsExplicitRetry(t *testing.T) {
	for _, retarget := range []bool{false, true} {
		t.Run(fmt.Sprint(retarget), func(t *testing.T) {
			f := newUpgradeFixture(t)
			f.native.verifyErr = ErrInstallationMigrationRequired
			first, code := f.run(t, false)
			if code != 1 || first.Upgrade == nil || f.native.inspections != 0 || f.native.ensures != 0 {
				t.Fatalf("initial verification fixture advanced Native control: code=%d progress=%+v", code, first.Upgrade)
			}
			before, err := f.client.ProfileMigrationSource(boundedTestContext(t), f.profile)
			if err != nil || before.DelegateExecutable != f.oldExecutable {
				t.Fatalf("read-only verification changed saved selection: %v", err)
			}
			f.native.verifyErr = nil
			if retarget {
				retargetGUIUpgradeFixture(t, f)
			}
			assertGUIInitialRetryCompleted(t, f, first)
			f.assertPreserved(t)
		})
	}
}

func assertGUIInitialRetryCompleted(t *testing.T, f *upgradeFixture, first Response) {
	t.Helper()
	second, code := f.run(t, false)
	if code != 0 || second.Upgrade == nil || second.Upgrade.Phase != "complete" || f.gui.starts != 2 || f.native.ensures != 0 {
		t.Fatalf("known pre-Ensure verification failure locked out explicit retry: code=%d failure=%s starts=%d ensures=%d first_phase=%s", code, second.Failure, f.gui.starts, f.native.ensures, first.Upgrade.Phase)
	}
	request := f.gui.requests[1]
	if request.Executable != f.newExecutable || request.SavedSource.Executable != f.oldExecutable || request.PATHSource != "prior_receipt" || request.ExpectedOperation != "" || request.NoNewNativeOperation {
		t.Fatal("fresh retry changed saved source, retained PATH or Native authority")
	}
}

func retargetGUIUpgradeFixture(t *testing.T, f *upgradeFixture) {
	t.Helper()
	f.newExecutable = filepath.Join(filepath.Dir(filepath.Dir(f.newExecutable)), "corrected-keg", "delegate")
	if err := os.Mkdir(filepath.Dir(f.newExecutable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.newExecutable, []byte("#!/bin/sh\nexit 64\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.dependencies.Executable = f.newExecutable
}

func TestGUIUpgradeKnownPostCASServiceFailureRetriesRealProfile(t *testing.T) {
	for _, stage := range []string{"install", "start"} {
		t.Run(stage, func(t *testing.T) {
			f := newUpgradeFixture(t)
			runner := guiSDKControl(t, f)
			f.services.fault = stage
			first, code := f.run(t, false)
			if code == 0 || first.Upgrade == nil || !first.Upgrade.DeviceIdentityPreserved {
				t.Fatalf("fixture did not reach post-CAS failure: code=%d response=%+v", code, first.Upgrade)
			}
			current, err := f.client.ProfileMigrationSource(boundedTestContext(t), f.profile)
			if err != nil || current.DelegateExecutable != f.newExecutable {
				t.Fatalf("real SDK source was not committed before failure: %v", err)
			}
			f.services.fault = ""
			second, code := f.run(t, false)
			if code != 0 || second.Upgrade.Phase != "complete" || runner.starts != 2 || f.native.ensures != 0 {
				t.Fatalf("known closed service failure became permanent uncertainty or new Native operation: code=%d starts=%d ensures=%d", code, runner.starts, f.native.ensures)
			}
			if !runner.requests[1].NoNewNativeOperation || runner.requests[1].SavedSource.Executable != f.newExecutable || runner.requests[1].PATHSource != "prior_receipt" {
				t.Fatal("retry lost committed source, PATH or authority reduction")
			}
			f.assertPreserved(t)
		})
	}
}

func TestGUIUpgradeReviewRequiredSurvivesResultTransport(t *testing.T) {
	f := newUpgradeFixture(t)
	operation := "upgrade-v1:" + strings.Repeat("a", 32)
	f.native.transition = NativeTransition{OperationID: operation, Phase: "migrating", Pending: true}
	f.native.ensureErr = ErrNativeUpgradeReviewRequired
	response, code := f.run(t, false)
	if code != 1 || response.Failure != "gateway_upgrade_review_required" || response.NextAction != "stop_and_report" || response.Upgrade.Supervisor.OperationID != operation {
		t.Fatalf("GUI result lost exact review refusal: code=%d response=%+v", code, response)
	}
	source, err := f.client.ProfileMigrationSource(boundedTestContext(t), f.profile)
	if err != nil || source.DelegateExecutable != f.oldExecutable || len(f.services.calls) != 1 || f.services.calls[0].action != "uninstall" || f.native.ensures != 1 {
		t.Fatalf("review refusal committed source or launched service: %v", err)
	}
	assertGUIReviewReceipt(t, f, operation)
	f.assertPreserved(t)
}

func assertGUIReviewReceipt(t *testing.T, f *upgradeFixture, operation string) {
	t.Helper()
	request := f.gui.requests[0]
	directory := filepath.Join(f.profile, ".gui-upgrade", request.Invocation)
	for _, name := range []string{"joined", "cleaned"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(directory, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result gatewayservice.GUIUpgradeResult
	if err = json.Unmarshal(data, &result); err != nil || result.OperationID != operation || !result.Pending || result.ExitCode != 1 {
		t.Fatalf("cleaned receipt lost retained exactA: %+v %v", result, err)
	}
}

func TestGUIUpgradeNotStartedPublicErrorRequiresCleanOutcome(t *testing.T) {
	for _, cause := range []error{nil, gatewayservice.ErrManager, gatewayservice.ErrGUIUpgradeRecord, context.Canceled, context.DeadlineExceeded} {
		err := errors.Join(&gatewayservice.GUIUpgradeNotStartedError{ExitCode: 1}, cause)
		failure, next, code, matched := guiUpgradePublicError(err)
		if cause == nil {
			if !matched || code != 3 || failure != "gateway_upgrade_not_started" || next != "retry_gateway_upgrade_same_profile" {
				t.Fatalf("known not-started outcome lost supported retry: %s %s %d", failure, next, code)
			}
		} else if matched && code == 3 {
			t.Fatalf("mixed not-started cause became retry advice: %v", cause)
		}
	}
}

func TestGUIUpgradeNoNewNativeOperationRejectsPendingB(t *testing.T) {
	f := newUpgradeFixture(t)
	f.native.transition = NativeTransition{OperationID: "upgrade-v1:" + strings.Repeat("b", 32), Phase: "draining", Pending: true}
	call := invocation{dependencies: f.dependencies, noNewNativeOperation: true}
	local := deviceclient.LocalContextInfo{DelegateExecutable: f.oldExecutable, DelegateRoot: f.root, DelegateRootContext: f.rootToken}
	err := call.prepareUpgradeNative(boundedTestContext(t), local, f.newExecutable, &UpgradeObservation{})
	if !errors.Is(err, task.ErrRequestConflict) || f.native.ensures != 0 {
		t.Fatalf("reduced retry authority selected/completed unrelatedB: %v", err)
	}
}

type guiFaultNative struct {
	NativeInstallation
	observed NativeTransition
	cause    error
}

func (n guiFaultNative) Inspect(context.Context, string, string) (NativeTransition, error) {
	return n.observed, n.cause
}

func TestGUIUpgradeNoNewNativeOperationPreservesMixedInspectFault(t *testing.T) {
	f := newUpgradeFixture(t)
	fault := errors.New("canonical close fault")
	f.dependencies.NativeInstallation = guiFaultNative{NativeInstallation: f.native, observed: NativeTransition{OperationID: "upgrade-v1:" + strings.Repeat("b", 32), Pending: true, Phase: "draining"}, cause: fault}
	call := invocation{dependencies: f.dependencies, noNewNativeOperation: true}
	progress := &UpgradeObservation{}
	local := deviceclient.LocalContextInfo{DelegateExecutable: f.oldExecutable, DelegateRoot: f.root, DelegateRootContext: f.rootToken}
	err := call.prepareUpgradeNative(boundedTestContext(t), local, f.newExecutable, progress)
	if !errors.Is(err, fault) || !errors.Is(err, task.ErrRequestConflict) || progress.Supervisor != (NativeTransition{}) || f.native.ensures != 0 {
		t.Fatalf("mixed inspect fault exposed unrelatedB as retry authority: observed=%+v err=%v", progress.Supervisor, err)
	}
}
