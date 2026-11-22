package gatewayservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
	"golang.org/x/sys/unix"
)

func TestGUIUpgradeRuntimePATHUsesValidatedInstalledConfiguration(t *testing.T) {
	f := newServiceFixture(t, MacOS)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	want := f.manager.config.Path
	c := f.manager.config
	c.Path = "/ambient/gui/only"
	reader, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	path, found, err := reader.RuntimePATH(f.ctx)
	if err != nil || !found || path != want {
		t.Fatalf("installed configuration lost to ambient PATH: found=%t path=%q err=%v", found, path, err)
	}
	if err = os.WriteFile(f.manager.unitPath(), []byte("foreign definition"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reader.RuntimePATH(f.ctx); !errors.Is(err, ErrForeignService) {
		t.Fatalf("foreign definition permitted PATH fallback: %v", err)
	}
}

type guiTestJob struct {
	running bool
	exit    int
}

type guiTestRunner struct {
	mu            sync.Mutex
	manager       *Manager
	base          Runner
	jobs          map[string]guiTestJob
	starts        int
	wg            sync.WaitGroup
	child         func(context.Context, GUIUpgradeRequest) (GUIUpgradeResult, error)
	childErr      error
	printFailure  int
	prints        int
	claimPID      int
	claimGate     func()
	claimRefusal  bool
	beforeBootout func() error
	afterBootout  func() error
}

type guiClaimGateKey struct{}

func (r *guiTestRunner) Probe(ctx context.Context, executable string, args []string) (int, error) {
	if len(args) == 2 && strings.Contains(args[1], ".upgrade.") {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, found := r.jobs[args[1]]; found {
			return 0, ctx.Err()
		}
		return 113, ctx.Err()
	}
	return r.base.Probe(ctx, executable, args)
}

func (r *guiTestRunner) Run(ctx context.Context, executable string, args []string) (Result, error) {
	if args[0] == "bootstrap" && strings.Contains(args[2], ".gui-upgrade") {
		request, err := r.manager.readGUIRequest(filepath.Base(filepath.Dir(args[2])))
		if err != nil {
			return Result{}, err
		}
		target := r.manager.guiUpgradeTarget(request)
		r.mu.Lock()
		r.starts++
		r.jobs[target] = guiTestJob{running: true}
		r.mu.Unlock()
		r.wg.Add(1)
		child, cancel := context.WithDeadline(context.WithoutCancel(ctx), request.Deadline)
		go func() {
			defer cancel()
			r.runChild(child, request)
		}()
		return Result{}, nil
	}
	if len(args) == 2 && strings.Contains(args[1], ".upgrade.") {
		return r.runGUIObservation(ctx, args)
	}
	return r.base.Run(ctx, executable, args)
}

func (r *guiTestRunner) runGUIObservation(ctx context.Context, args []string) (Result, error) {
	if args[0] == "bootout" && r.beforeBootout != nil {
		if err := r.beforeBootout(); err != nil {
			return Result{}, err
		}
	}
	result := r.observe(args)
	if args[0] == "bootout" && r.afterBootout != nil {
		if err := r.afterBootout(); err != nil {
			return result, err
		}
	}
	if ctx.Value(guiClaimGateKey{}) != nil && r.claimGate != nil {
		r.claimGate()
	}
	return result, ctx.Err()
}

func (r *guiTestRunner) observe(args []string) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	if args[0] == "print" {
		r.prints++
	}
	if args[0] == "print" && r.printFailure != 0 {
		return Result{ExitCode: r.printFailure}
	}
	job, found := r.jobs[args[1]]
	if !found {
		return Result{ExitCode: 113}
	}
	if args[0] == "bootout" {
		delete(r.jobs, args[1])
		return Result{}
	}
	if job.running {
		pid := os.Getpid()
		if r.claimPID != 0 {
			pid = r.claimPID
		}
		return Result{Stdout: fmt.Appendf(nil, "state = running\npid = %d\n", pid)}
	}
	return Result{Stdout: fmt.Appendf(nil, "state = not running\nlast exit code = %d\n", job.exit)}
}

func TestGUIUpgradeObservationBoundsManagerPolling(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	var selected GUIUpgradeRequest
	if err := f.manager.WithProfileLock(f.ctx, func() error {
		var err error
		selected, _, err = f.manager.prepareGUIUpgrade(f.ctx, request)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runner.jobs[f.manager.guiUpgradeTarget(selected)] = guiTestJob{running: true}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		returned := make(chan error, 1)
		go func() { _, err := f.manager.observeGUIUpgrade(ctx, selected); returned <- err }()
		synctest.Wait()
		time.Sleep(time.Minute)
		if err := <-returned; !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrGUIUpgradeUnknown) {
			t.Fatalf("bounded observer lost original deadline: %v", err)
		}
		runner.mu.Lock()
		prints := runner.prints
		runner.mu.Unlock()
		if prints < 2 || prints > 70 {
			t.Fatalf("one minute of waiting launched %d manager observations; want 2..70", prints)
		}
	})
}

func TestGUIUpgradePreclaimRefusalRequiresExplicitNewInvocation(t *testing.T) {
	for _, retarget := range []bool{false, true} {
		t.Run(fmt.Sprint(retarget), func(t *testing.T) { testGUIPreclaimRetry(t, retarget) })
	}
}

func testGUIPreclaimRetry(t *testing.T, retarget bool) {
	t.Helper()
	f, runner, request := newGUIUpgradeTest(t)
	runner.claimPID, runner.claimRefusal = -1, true
	result, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err == nil || errors.Is(err, ErrGUIUpgradeUnknown) || len(result.Response) != 0 || runner.starts != 1 {
		t.Fatalf("proven preclaim refusal stayed unknown or relaunched: starts=%d err=%v", runner.starts, err)
	}
	first, _, readErr := f.manager.activeGUIUpgrade()
	if readErr != nil {
		t.Fatal(readErr)
	}
	runner.claimPID, runner.claimRefusal = 0, false
	if retarget {
		request.Executable = newGUIRetargetExecutable(t)
	}
	result, err = f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || result.ExitCode != 0 || runner.starts != 2 {
		t.Fatalf("explicit retry could not replace proven no-start invocation: starts=%d err=%v", runner.starts, err)
	}
	next, _, readErr := f.manager.activeGUIUpgrade()
	if readErr != nil || next.Invocation == first.Invocation || next.SavedSource != first.SavedSource || next.RuntimePATH != first.RuntimePATH {
		t.Fatalf("no-start retry reused old invocation or lost source/PATH: %v", readErr)
	}
}

func TestGUIUpgradeNoStartRetryRetainsIntent(t *testing.T) {
	for _, kind := range []string{"exact operation", "reduced authority", "source"} {
		t.Run(kind, func(t *testing.T) {
			f, runner, request := newGUIUpgradeTest(t)
			switch kind {
			case "exact operation":
				request.ExpectedOperation = "upgrade-v1:" + strings.Repeat("a", 32)
			case "reduced authority":
				request.NoNewNativeOperation = true
			}
			runner.claimPID, runner.claimRefusal = -1, true
			_, err := f.manager.RunGUIUpgrade(f.ctx, request)
			var notStarted *GUIUpgradeNotStartedError
			if !errors.As(err, &notStarted) {
				t.Fatal(err)
			}
			if kind == "source" {
				request.SavedSource.Executable += "-other"
			} else {
				request.Executable = newGUIRetargetExecutable(t)
			}
			if _, err = f.manager.RunGUIUpgrade(f.ctx, request); !errors.Is(err, task.ErrRequestConflict) || runner.starts != 1 {
				t.Fatalf("known no-start relaxed %s: starts=%d err=%v", kind, runner.starts, err)
			}
		})
	}
}

func TestGUIUpgradeNoStartObservationFencesLateClaim(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	var selected GUIUpgradeRequest
	if err := f.manager.WithProfileLock(f.ctx, func() error {
		var err error
		selected, _, err = f.manager.prepareGUIUpgrade(f.ctx, request)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	target := f.manager.guiUpgradeTarget(selected)
	runner.jobs[target] = guiTestJob{running: true}
	checked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	runner.claimGate = func() { close(checked); <-release }
	returned := make(chan error, 1)
	var child sync.WaitGroup
	child.Go(func() {
		claim, err := f.manager.ClaimGUIUpgrade(context.WithValue(f.ctx, guiClaimGateKey{}, true), selected)
		if err == nil {
			err = claim.Finish(guiTestReply(selected, 0, "", "", false))
		}
		returned <- err
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); child.Wait() })
	select {
	case <-checked:
	case <-f.ctx.Done():
		t.Fatal("claim did not reach supplementary PID observation")
	}
	runner.mu.Lock()
	runner.jobs[target] = guiTestJob{exit: 1}
	runner.mu.Unlock()
	_, observationErr := f.manager.RunGUIUpgrade(f.ctx, request)
	releaseOnce.Do(func() { close(release) })
	if claimErr := <-returned; claimErr == nil {
		t.Fatalf("late child acquired effect authority after no-start observation: observer=%v", observationErr)
	}
	if observationErr == nil || errors.Is(observationErr, ErrGUIUpgradeUnknown) {
		t.Fatalf("positive no-start observation remained unknown: %v", observationErr)
	}
}

func TestGUIUpgradeNoStartCleanupCrashResumesSameSeed(t *testing.T) {
	for _, afterBootout := range []bool{false, true} {
		t.Run(fmt.Sprint(afterBootout), func(t *testing.T) { testGUINoStartCleanupCrash(t, afterBootout) })
	}
}

func stageGUINoStartJob(t *testing.T) (*serviceFixture, *guiTestRunner, GUIUpgradeRequest) {
	t.Helper()
	f, runner, request := newGUIUpgradeTest(t)
	request.ExpectedOperation = "upgrade-v1:" + strings.Repeat("a", 32)
	if err := f.manager.WithProfileLock(f.ctx, func() error {
		var err error
		request, _, err = f.manager.prepareGUIUpgrade(f.ctx, request)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The OS edge supplies a previously launched exact-job exit. No real
	// process or Native operation is created by this private-record fixture.
	runner.jobs[f.manager.guiUpgradeTarget(request)] = guiTestJob{exit: 1}
	return f, runner, request
}

func testGUINoStartCleanupCrash(t *testing.T, afterBootout bool) {
	t.Helper()
	f, runner, request := newGUIUpgradeTest(t)
	request.ExpectedOperation = "upgrade-v1:" + strings.Repeat("a", 32)
	runner.claimPID, runner.claimRefusal = -1, true
	proofSeen, crash := injectGUINoStartCleanupCrash(f.manager, runner, afterBootout)
	_, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if !errors.Is(err, crash) || !*proofSeen || runner.starts != 1 {
		t.Fatalf("cleanup preceded durable no-start proof or lost its fault: starts=%d proof=%t err=%v", runner.starts, *proofSeen, err)
	}
	first, _, err := f.manager.activeGUIUpgrade()
	if err != nil {
		t.Fatal(err)
	}
	runner.beforeBootout, runner.afterBootout = nil, nil
	_, err = f.manager.RunGUIUpgrade(f.ctx, request)
	var notStarted *GUIUpgradeNotStartedError
	if !errors.As(err, &notStarted) || notStarted.ExitCode != 1 || runner.starts != 1 {
		t.Fatalf("cleanup crash lost positive proof or rebootstrapped: starts=%d err=%v", runner.starts, err)
	}
	if cleaned, readErr := f.manager.guiMarker(first, "cleaned"); readErr != nil || !cleaned {
		t.Fatalf("cleanup did not resume from durable proof: %v", readErr)
	}
	runner.claimPID, runner.claimRefusal = 0, false
	runner.child = func(_ context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		return guiTestReply(r, 0, r.ExpectedOperation, "completed", false), nil
	}
	result, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || result.OperationID != request.ExpectedOperation || runner.starts != 2 {
		t.Fatalf("explicit sameA retry changed operation: starts=%d err=%v", runner.starts, err)
	}
	assertGUINoStartSeedPreserved(t, f.manager, first)
}

func injectGUINoStartCleanupCrash(manager *Manager, runner *guiTestRunner, afterBootout bool) (*bool, error) {
	proofSeen := new(bool)
	crash := errors.New("owned cleanup interruption")
	runner.beforeBootout = func() error {
		active, _, err := manager.activeGUIUpgrade()
		if err != nil {
			return err
		}
		_, *proofSeen, err = manager.readGUINotStarted(active)
		if !afterBootout {
			err = errors.Join(err, crash)
		}
		return err
	}
	if afterBootout {
		runner.afterBootout = func() error { return crash }
	}
	return proofSeen, crash
}

func assertGUINoStartSeedPreserved(t *testing.T, manager *Manager, request GUIUpgradeRequest) {
	t.Helper()
	next, _, err := manager.activeGUIUpgrade()
	if err != nil || next.Invocation == request.Invocation || next.SavedSource != request.SavedSource || next.ExpectedOperation != request.ExpectedOperation || next.RuntimePATH != request.RuntimePATH {
		t.Fatalf("cleanup recovery lost immutable seed: %v", err)
	}
}

func TestGUIUpgradeNoStartRolloverRecoversPublishedSeed(t *testing.T) {
	f, runner, request := stageGUINoStartJob(t)
	if err := f.manager.recordGUINotStarted(f.ctx, request, guiNotStartedProof{Invocation: request.Invocation, ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.cleanupGUIJob(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.WithProfileLock(f.ctx, func() error {
		stageGUIReconciliationCrash(t, f.manager, request, request.ExpectedOperation, true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runner.child = func(_ context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		return guiTestReply(r, 0, r.ExpectedOperation, "completed", false), nil
	}
	result, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || result.OperationID != request.ExpectedOperation || runner.starts != 1 {
		t.Fatalf("published no-start seed could not resume exactA: %v", err)
	}
}

func TestGUIUpgradeNoStartEvidenceRemainsConservative(t *testing.T) {
	for _, kind := range []string{"absent job", "zero exit", "claimed", "held alive", "bad alive", "bad claim", "bad proof", "proof and claim", "proof and result", "proof and held alive", "manager fault"} {
		t.Run(kind, func(t *testing.T) {
			f, runner, request := stageGUINoStartJob(t)
			wanted := setGUINoStartFault(t, f.manager, runner, request, kind)
			_, err := f.manager.RunGUIUpgrade(f.ctx, request)
			var notStarted *GUIUpgradeNotStartedError
			if !errors.Is(err, wanted) || errors.As(err, &notStarted) || runner.starts != 0 {
				t.Fatalf("invalid %s became positive no-start: starts=%d err=%v", kind, runner.starts, err)
			}
		})
	}
}

func setGUINoStartFault(t *testing.T, manager *Manager, runner *guiTestRunner, request GUIUpgradeRequest, kind string) error {
	t.Helper()
	var err error
	wanted := ErrGUIUpgradeRecord
	switch kind {
	case "absent job":
		delete(runner.jobs, manager.guiUpgradeTarget(request))
		wanted = ErrGUIUpgradeUnknown
	case "zero exit":
		runner.jobs[manager.guiUpgradeTarget(request)] = guiTestJob{exit: 0}
	case "claimed":
		err = manager.writeGUIMarker(request, "claim")
		wanted = ErrGUIUpgradeUnknown
	case "held alive":
		holdGUINoStartAlive(t, manager.guiFile(request, "alive"))
		wanted = ErrGUIUpgradeUnknown
	case "bad alive":
		err = os.WriteFile(manager.guiFile(request, "alive"), nil, 0o644)
	case "bad claim":
		err = os.WriteFile(manager.guiFile(request, "claim"), []byte("{"), 0o600)
	case "bad proof":
		err = guiWriteJSON(manager.guiFile(request, "not_started"), guiNotStartedProof{Invocation: request.Invocation})
	case "proof and claim":
		err = errors.Join(guiWriteJSON(manager.guiFile(request, "not_started"), guiNotStartedProof{Invocation: request.Invocation, ExitCode: 1}), manager.writeGUIMarker(request, "claim"))
	case "proof and result":
		err = errors.Join(guiWriteJSON(manager.guiFile(request, "not_started"), guiNotStartedProof{Invocation: request.Invocation, ExitCode: 1}), guiWriteJSON(manager.guiFile(request, "result.json"), guiTestReply(request, 0, "", "", false)))
	case "proof and held alive":
		err = errors.Join(guiWriteJSON(manager.guiFile(request, "not_started"), guiNotStartedProof{Invocation: request.Invocation, ExitCode: 1}), manager.writeGUIMarker(request, "cleaned"))
		holdGUINoStartAlive(t, manager.guiFile(request, "alive"))
		wanted = ErrGUIUpgradeUnknown
	case "manager fault":
		runner.printFailure = 7
		wanted = ErrManager
	}
	if err != nil {
		t.Fatal(err)
	}
	return wanted
}

func holdGUINoStartAlive(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func TestGUIUpgradeInitialVerificationRetryPreservesGuards(t *testing.T) {
	for _, kind := range []string{"legacy phase", "post phase", "exact operation", "reduced authority", "changed source", "changed root", "handoff observation"} {
		t.Run(kind, func(t *testing.T) { testGUIInitialRetryGuard(t, kind) })
	}
}

func testGUIInitialRetryGuard(t *testing.T, kind string) {
	t.Helper()
	f, runner, request := newGUIUpgradeTest(t)
	phase := "verify_initial_installation"
	switch kind {
	case "legacy phase":
		phase = "verify_selected_installation"
	case "post phase":
		phase = "verify_transitioned_installation"
	case "exact operation":
		request.ExpectedOperation = "upgrade-v1:" + strings.Repeat("a", 32)
	case "reduced authority":
		request.NoNewNativeOperation = true
	}
	runner.child = func(_ context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		return guiVerificationFailure(r, phase, kind == "handoff observation")
	}
	if _, err := f.manager.RunGUIUpgrade(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "changed source":
		request.SavedSource.Executable += "-other"
	case "changed root":
		request.Root += "-other"
	}
	if kind == "exact operation" || kind == "reduced authority" {
		request.Executable = newGUIRetargetExecutable(t)
	}
	if _, err := f.manager.RunGUIUpgrade(f.ctx, request); err == nil || runner.starts != 1 {
		t.Fatalf("initial verification recovery relaxed %s: starts=%d err=%v", kind, runner.starts, err)
	}
}

func guiVerificationFailure(request GUIUpgradeRequest, phase string, handoff bool) (GUIUpgradeResult, error) {
	result := guiTestReply(request, 1, "", "", false)
	value, err := result.publicUpgrade()
	if err != nil {
		return result, err
	}
	value.Upgrade.Phase = phase
	value.Upgrade.Native.RootContextMatches, value.Upgrade.Native.CapabilitiesVerified = false, false
	value.Upgrade.Supervisor.HandoffNeeded = handoff
	result.Response, err = json.Marshal(value)
	return result, err
}

func newGUIRetargetExecutable(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "corrected-delegate")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 64\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable
}

func TestGUIUpgradeInitialVerificationRetargetCrashRecovery(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	runner.child = func(_ context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		return guiVerificationFailure(r, "verify_initial_installation", false)
	}
	if _, err := f.manager.RunGUIUpgrade(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	old, _, err := f.manager.activeGUIUpgrade()
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.Executable = newGUIRetargetExecutable(t)
	if err = f.manager.WithProfileLock(f.ctx, func() error {
		next = stageGUIReconciliationCrash(t, f.manager, next, "", true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	request.Executable = next.Executable
	runner.child = nil
	result, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || result.ExitCode != 0 || runner.starts != 2 {
		t.Fatalf("fresh retarget publication crash lost recovery: starts=%d err=%v", runner.starts, err)
	}
	active, found, err := f.manager.activeGUIUpgrade()
	if err != nil || !found || active.SavedSource != old.SavedSource || active.RuntimePATH != old.RuntimePATH || active.ExpectedOperation != "" || active.NoNewNativeOperation {
		t.Fatalf("fresh retarget crash changed source, PATH or authority: %v", err)
	}
}

func TestGUIUpgradeAliveRecordFaultIsNotPolledIntoDeadline(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	runner.child = func(_ context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		err := os.Chmod(f.manager.guiFile(r, "alive"), 0o644)
		return guiTestReply(r, 0, "", "", false), err
	}
	_, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if !errors.Is(err, ErrGUIUpgradeRecord) || IsPending(err) || errors.Is(err, context.DeadlineExceeded) || f.ctx.Err() != nil {
		t.Fatalf("concrete alive fault was lost to pending/expiry: %v", err)
	}
}

func TestGUIUpgradeManagerPrintFaultRemainsHardWithAndWithoutResult(t *testing.T) {
	for _, withResult := range []bool{false, true} {
		t.Run(fmt.Sprint(withResult), func(t *testing.T) {
			f, runner, request := newGUIPrintFailureFixture(t, withResult)
			runner.printFailure = 7
			_, err := f.manager.RunGUIUpgrade(f.ctx, request)
			if !errors.Is(err, ErrManager) || !errors.Is(err, ErrGUIUpgradeUnknown) {
				t.Fatalf("manager failure became pure uncertainty: %v", err)
			}
		})
	}
}

func (r *guiTestRunner) runChild(ctx context.Context, request GUIUpgradeRequest) {
	defer r.wg.Done()
	claim, err := r.manager.ClaimGUIUpgrade(ctx, request)
	result := guiTestReply(request, 0, "", "", false)
	if err == nil {
		if r.child != nil {
			result, err = r.child(ctx, request)
		}
		err = errors.Join(err, claim.Finish(result))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.claimRefusal || !errors.Is(err, ErrGUIRequired) {
		r.childErr = errors.Join(r.childErr, err)
	}
	code := result.ExitCode
	if err != nil {
		code = 1
	}
	r.jobs[r.manager.guiUpgradeTarget(request)] = guiTestJob{exit: code}
}

func newGUIUpgradeTest(t *testing.T) (*serviceFixture, *guiTestRunner, GUIUpgradeRequest) {
	t.Helper()
	f := newServiceFixture(t, MacOS)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	runner := &guiTestRunner{manager: f.manager, base: f.runner, jobs: map[string]guiTestJob{}}
	f.manager.config.Runner = runner
	t.Cleanup(func() {
		runner.wg.Wait()
		if runner.childErr != nil {
			t.Error(runner.childErr)
		}
	})
	root := filepath.Dir(f.manager.config.ProfileDirectory)
	request := GUIUpgradeRequest{Profile: f.manager.config.ProfileDirectory, Root: root, ExpectedRoot: "opaque-original-root", Executable: f.manager.config.Executable, Accepted: true, SavedSource: GUIUpgradeSource{Executable: f.manager.config.Executable, Root: root, Context: "opaque-original-root"}}
	return f, runner, request
}

func TestGUIUpgradeSingleFlightReleasesProfileCoordination(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	wantPATH := f.manager.config.Path
	f.manager.config.Path = "/ambient/gui/only"
	entered, release := make(chan struct{}), make(chan struct{})
	runner.child = func(ctx context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		err := WithProfileLock(ctx, r.Profile, func() error {
			if r.RuntimePATH != wantPATH || r.PATHSource != "existing_definition" {
				return errors.New("ambient PATH replaced installed configuration")
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		return guiTestReply(r, 0, "", "", false), err
	}
	type observed struct {
		result GUIUpgradeResult
		err    error
	}
	results := make(chan observed, 2)
	var callers sync.WaitGroup
	call := func() { result, err := f.manager.RunGUIUpgrade(f.ctx, request); results <- observed{result, err} }
	callers.Go(call)
	select {
	case <-entered:
	case <-f.ctx.Done():
		t.Fatal("GUI child could not acquire the real profile lock while parent waited")
	}
	// A second observer may join an accepted live job. Before bootstrap has
	// returned, an attempted-but-absent job is conservatively unknown instead.
	callers.Go(call)
	close(release)
	callers.Wait()
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.result.Invocation != second.result.Invocation || runner.starts != 1 {
		t.Fatalf("concurrent explicit observation duplicated bootstrap: starts=%d first=%v second=%v", runner.starts, first.err, second.err)
	}
}

func TestGUIUpgradeRolloverRecoversBothPublicationWindows(t *testing.T) {
	for _, afterPointer := range []bool{false, true} {
		t.Run(fmt.Sprint(afterPointer), func(t *testing.T) { testGUIRolloverWindow(t, afterPointer) })
	}
}

func testGUIRolloverWindow(t *testing.T, afterPointer bool) {
	t.Helper()
	f, runner, request := newGUIUpgradeTest(t)
	operation := "upgrade-v1:" + strings.Repeat("b", 32)
	runner.child = func(_ context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		return guiTestReply(r, 3, operation, "migrating", true), nil
	}
	if _, err := f.manager.RunGUIUpgrade(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	old, found, err := f.manager.activeGUIUpgrade()
	if err != nil || !found {
		t.Fatal(err)
	}
	next := stageGUIReconciliationCrash(t, f.manager, old, operation, afterPointer)
	// Simulated process loss here: no attempted marker means this staging
	// invocation was never launchable. The old pending A seed still exists.
	if _, err = os.Stat(f.manager.guiFile(old, "request.json")); err != nil {
		t.Fatal("old pending seed disappeared before replacement publication")
	}
	result, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || runner.starts != 2 || result.OperationID != operation || result.Invocation == next.Invocation {
		t.Fatalf("crash window lost same-A recovery or replayed staged bootstrap: starts=%d result=%+v err=%v", runner.starts, result, err)
	}
	active, _, err := f.manager.activeGUIUpgrade()
	if err != nil || active.ExpectedOperation != operation || active.RuntimePATH != old.RuntimePATH {
		t.Fatal("recovered invocation rebased pending A or PATH")
	}
}

func TestGUIUpgradeDuplicateChildClaimRefusesBeforeEffects(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	runner.child = func(ctx context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		duplicate, err := f.manager.ClaimGUIUpgrade(ctx, r)
		if err == nil || duplicate != nil {
			return GUIUpgradeResult{}, errors.New("stale claim was accepted")
		}
		return guiTestReply(r, 0, "", "", false), nil
	}
	if _, err := f.manager.RunGUIUpgrade(f.ctx, request); err != nil || runner.starts != 1 {
		t.Fatalf("exclusive child claim failed: starts=%d err=%v", runner.starts, err)
	}
}

func TestGUIUpgradeMissingGUIAndCanceledCallerCreateNoSlot(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			f, runner, request := newGUIUpgradeTest(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			want := ErrGUIRequired
			if canceled {
				cancel()
				want = context.Canceled
			} else {
				f.runner.probeResults = []probeResult{{code: 113}}
			}
			if _, err := f.manager.RunGUIUpgrade(ctx, request); !errors.Is(err, want) || runner.starts != 0 {
				t.Fatalf("invalid caller launched work: starts=%d err=%v", runner.starts, err)
			}
			if _, err := os.Lstat(f.manager.guiSlot()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid caller created job resources: %v", err)
			}
		})
	}
}

func TestGUIUpgradeParentCancellationDoesNotRenewOrCancelChildDeadline(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	runner.child = func(ctx context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(r.Deadline) {
			return GUIUpgradeResult{}, errors.New("child renewed original deadline")
		}
		close(entered)
		select {
		case <-release:
			return guiTestReply(r, 0, "", "", false), ctx.Err()
		case <-ctx.Done():
			return GUIUpgradeResult{}, ctx.Err()
		}
	}
	parent, cancel := context.WithCancel(f.ctx)
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := f.manager.RunGUIUpgrade(parent, request); returned <- err }()
	select {
	case <-entered:
	case <-f.ctx.Done():
		t.Fatal("child not entered")
	}
	cancel()
	if err := <-returned; !errors.Is(err, ErrGUIUpgradeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation lost uncertainty or its cause: %v", err)
	}
	runner.mu.Lock()
	alive := false
	for _, job := range runner.jobs {
		alive = alive || job.running
	}
	runner.mu.Unlock()
	if !alive {
		t.Fatal("observation cancellation stopped the accepted GUI child")
	}
	releaseOnce.Do(func() { close(release) })
	runner.wg.Wait()
	result, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || result.ExitCode != 0 || runner.starts != 1 {
		t.Fatalf("lost reply was relaunched or could not be observed: starts=%d err=%v", runner.starts, err)
	}
}

func TestGUIUpgradePrivateRequestRejectsMismatchedOrUnsafeFile(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "unknown field", "root pin", "locator"} {
		t.Run(kind, func(t *testing.T) {
			f, _, request := newGUIUpgradeTest(t)
			var saved GUIUpgradeRequest
			if err := f.manager.WithProfileLock(f.ctx, func() error { var err error; saved, _, err = f.manager.prepareGUIUpgrade(f.ctx, request); return err }); err != nil {
				t.Fatal(err)
			}
			path := f.manager.guiFile(saved, "request.json")
			mutateGUIPrivateRequest(t, kind, path, saved)
			locator := path
			if kind == "locator" {
				locator = filepath.Join(filepath.Dir(path), "other.json")
			}
			if _, err := ReadGUIUpgradeRequest(locator, saved.Profile); err == nil {
				t.Fatal("unsafe private request accepted")
			}
		})
	}
}

func mutateGUIPrivateRequest(t *testing.T, kind, path string, request GUIUpgradeRequest) {
	t.Helper()
	var err error
	switch kind {
	case "mode":
		err = os.Chmod(path, 0o644)
	case "symlink":
		if err = os.Remove(path); err == nil {
			err = os.Symlink(filepath.Join(t.TempDir(), "not-request"), path)
		}
	case "root pin":
		request.ExpectedRoot += "-changed"
		var data []byte
		data, err = json.Marshal(request)
		if err == nil {
			err = os.WriteFile(path, data, 0o600)
		}
	case "unknown field":
		var data []byte
		data, err = os.ReadFile(path)
		if err == nil {
			data = bytes.TrimSpace(data)
			data = append(data[:len(data)-1], []byte(",\"outside\":true}")...)
			err = os.WriteFile(path, data, 0o600)
		}
	case "locator":
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestGUIUpgradePendingSeedSurvivesUninstallAndExplicitReconciliation(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	wantPATH := f.manager.config.Path
	operation := "upgrade-v1:" + strings.Repeat("a", 32)
	var first GUIUpgradeRequest
	runner.child = func(ctx context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		if r.ExpectedOperation == "" {
			first = r
			_, err := f.manager.Uninstall(ctx)
			return guiTestReply(r, 3, operation, "migrating", true), err
		}
		if r.ExpectedOperation != operation || r.RuntimePATH != wantPATH || r.PATHSource != "prior_receipt" || !r.Deadline.After(first.Deadline) {
			return GUIUpgradeResult{}, errors.New("explicit reconciliation lost exact seed or new caller deadline")
		}
		return guiTestReply(r, 0, operation, "complete", false), nil
	}
	short, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := f.manager.RunGUIUpgrade(short, request)
	if err != nil || !result.Pending || result.OperationID != operation {
		t.Fatalf("pending exact operation was lost: %+v %v", result, err)
	}
	runner.wg.Wait()
	f.manager.config.Path = "/different/ambient"
	result, err = f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || result.Pending || result.OperationID != operation || runner.starts != 2 || result.Invocation == first.Invocation {
		t.Fatalf("explicit same-A reconciliation failed: %+v starts=%d err=%v", result, runner.starts, err)
	}
	if _, err = os.Lstat(f.manager.guiRecord(first.Invocation)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("joined old attempt not retired after durable new seed: %v", err)
	}
}

func TestGUIUpgradeCompletedInvocationAllowsNextExplicitInstallation(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	first, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Executable, err = verifiedexec.LocatePath("/usr/bin/false")
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.manager.RunGUIUpgrade(f.ctx, request)
	if err != nil || runner.starts != 2 || next.Invocation == first.Invocation {
		t.Fatalf("completed old invocation prevented a new explicit installation: starts=%d err=%v", runner.starts, err)
	}
}

func TestGUIUpgradeUnknownAttemptNeverBootstrapsAgainOrChangesTarget(t *testing.T) {
	f, runner, request := newGUIUpgradeTest(t)
	var staged GUIUpgradeRequest
	err := f.manager.WithProfileLock(f.ctx, func() error {
		var prepareErr error
		staged, _, prepareErr = f.manager.prepareGUIUpgrade(f.ctx, request)
		return prepareErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.RunGUIUpgrade(f.ctx, request); !errors.Is(err, ErrGUIUpgradeUnknown) || runner.starts != 0 {
		t.Fatalf("uncertain attempted launch was repeated: starts=%d err=%v", runner.starts, err)
	}
	request.Executable, err = verifiedexec.LocatePath("/usr/bin/false")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.RunGUIUpgrade(f.ctx, request); !errors.Is(err, task.ErrRequestConflict) || runner.starts != 0 {
		t.Fatalf("unknown attempt was silently retargeted: %v", err)
	}
	active, found, err := f.manager.activeGUIUpgrade()
	if err != nil || !found || active != staged {
		t.Fatal("unknown request/target was not retained")
	}
}

func guiTestReply(request GUIUpgradeRequest, code int, operation, phase string, pending bool) GUIUpgradeResult {
	progress := &guiPublicUpgradeProgress{Phase: "complete"}
	if pending {
		progress.Phase = "supervisor_transition"
	}
	progress.Native.RootContextMatches, progress.Native.CapabilitiesVerified = !pending, !pending
	progress.Supervisor.OperationID, progress.Supervisor.Phase, progress.Supervisor.Pending = operation, phase, pending
	reply := guiPublicUpgrade{SchemaVersion: 1, Command: "gateway", Action: "upgrade", Profile: request.Profile, Upgrade: progress}
	if code != 0 {
		reply.Failure = "gateway_upgrade_pending"
	}
	payload, err := json.Marshal(reply)
	if err != nil {
		return GUIUpgradeResult{}
	}
	return GUIUpgradeResult{Version: 1, Invocation: request.Invocation, ExitCode: code, Response: payload, OperationID: operation, Phase: phase, Pending: pending}
}

func TestGUIUpgradeRetainedAuthorityCannotAdoptConflictingObservation(t *testing.T) {
	f, _, base := newGUIUpgradeTest(t)
	base.Version, base.Invocation, base.RuntimePATH, base.PATHSource, base.Deadline = 1, strings.Repeat("c", 32), f.manager.config.Path, "existing_definition", time.Now().Add(time.Minute)
	a, b := "upgrade-v1:"+strings.Repeat("a", 32), "upgrade-v1:"+strings.Repeat("b", 32)
	for _, reduced := range []bool{false, true} {
		t.Run(fmt.Sprint(reduced), func(t *testing.T) {
			previous := base
			if reduced {
				previous.NoNewNativeOperation = true
			} else {
				previous.ExpectedOperation = a
			}
			observation := guiTestReply(previous, 3, b, "draining", true)
			if err := observation.validate(previous); !errors.Is(err, ErrGUIUpgradeRecord) {
				t.Fatalf("conflicting result/request accepted: %v", err)
			}
			next := base
			if err := next.retainGUIIntent(previous, observation); !errors.Is(err, ErrGUIUpgradeRecord) {
				t.Fatalf("conflicting observation gained authority: %v", err)
			}
		})
	}
	previous := base
	previous.NoNewNativeOperation = true
	knownFailure := guiTestReply(previous, 1, "", "", false)
	next := base
	if err := next.retainGUIIntent(previous, knownFailure); err != nil || !next.NoNewNativeOperation || next.ExpectedOperation != "" {
		t.Fatalf("known fault dropped retained reduction: %+v %v", next, err)
	}
}

func TestGUIUpgradeUnpublishedActiveReferenceCleanupPreservesPendingSeed(t *testing.T) {
	for _, kind := range []string{"partial", "mode", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) { testGUIUnpublishedReference(t, kind) })
	}
}

func testGUIUnpublishedReference(t *testing.T, kind string) {
	t.Helper()
	f, runner, request := newGUIUpgradeTest(t)
	operation := "upgrade-v1:" + strings.Repeat("a", 32)
	runner.child = func(_ context.Context, r GUIUpgradeRequest) (GUIUpgradeResult, error) {
		return guiTestReply(r, 3, operation, "migrating", true), nil
	}
	if _, err := f.manager.RunGUIUpgrade(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	before, found, err := f.manager.activeGUIUpgrade()
	if err != nil || !found {
		t.Fatal(err)
	}
	temporary := filepath.Join(f.manager.guiSlot(), ".publish-"+strings.Repeat("d", 32))
	createGUIStagingFault(t, kind, temporary)
	err = f.manager.WithProfileLock(f.ctx, f.manager.cleanGUIStaging)
	if kind == "partial" {
		if err != nil {
			t.Fatalf("never-launchable partial publication permanently fenced recovery: %v", err)
		}
		if _, statErr := os.Lstat(temporary); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("partial staging file retained")
		}
	} else if !errors.Is(err, ErrGUIUpgradeRecord) {
		t.Fatalf("unsafe publication entry accepted: %v", err)
	}
	assertGUIStagingSeed(t, f.manager, before, operation, runner.starts)
}

func createGUIStagingFault(t *testing.T, kind, path string) {
	t.Helper()
	var err error
	if kind == "partial" || kind == "mode" {
		err = os.WriteFile(path, []byte(`{"invocation":`), 0o600)
		if err == nil && kind == "mode" {
			err = os.Chmod(path, 0o644)
		}
	} else {
		target := filepath.Join(t.TempDir(), "retained")
		if err = os.WriteFile(target, []byte("retained"), 0o600); err == nil {
			if kind == "symlink" {
				err = os.Symlink(target, path)
			} else {
				err = os.Link(target, path)
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

func newGUIPrintFailureFixture(t *testing.T, withResult bool) (*serviceFixture, *guiTestRunner, GUIUpgradeRequest) {
	t.Helper()
	f, runner, request := newGUIUpgradeTest(t)
	if withResult {
		if _, err := f.manager.RunGUIUpgrade(f.ctx, request); err != nil {
			t.Fatal(err)
		}
		active, _, err := f.manager.activeGUIUpgrade()
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"cleaned", "joined"} {
			if err = os.Remove(f.manager.guiFile(active, name)); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		if err := f.manager.WithProfileLock(f.ctx, func() error { _, _, err := f.manager.prepareGUIUpgrade(f.ctx, request); return err }); err != nil {
			t.Fatal(err)
		}
	}
	return f, runner, request
}

func stageGUIReconciliationCrash(t *testing.T, manager *Manager, old GUIUpgradeRequest, operation string, afterPointer bool) GUIUpgradeRequest {
	t.Helper()
	next := old
	var err error
	next.Invocation, err = guiRandomID()
	if err != nil {
		t.Fatal(err)
	}
	next.ExpectedOperation, next.PATHSource = operation, "prior_receipt"
	if err = manager.stageGUIUpgrade(next); err != nil {
		t.Fatal(err)
	}
	if afterPointer {
		if err = manager.writeGUIActive(next); err != nil {
			t.Fatal(err)
		}
	}
	return next
}

func assertGUIStagingSeed(t *testing.T, manager *Manager, before GUIUpgradeRequest, operation string, starts int) {
	t.Helper()
	after, found, readErr := manager.activeGUIUpgrade()
	if readErr != nil || !found || after != before || starts != 1 {
		t.Fatalf("staging cleanup changed pendingA/PATH or bootstrap: starts=%d err=%v", starts, readErr)
	}
	result, found, readErr := manager.readGUIResult(after)
	if readErr != nil || !found || result.OperationID != operation || !result.Pending {
		t.Fatal("old pending result was discarded")
	}
}
