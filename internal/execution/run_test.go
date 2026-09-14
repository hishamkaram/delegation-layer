package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fixtureTask(t *testing.T, mode string, faults ...taskdir.FaultInjector) (*taskdir.TaskDir, *taskdir.StartPermit, Plan, []byte) {
	t.Helper()
	return fixtureTaskWithPlan(t, mode, nil, faults...)
}

func fixtureTaskWithPlan(t *testing.T, mode string, configure func(*task.MetaRecord, *Plan), faults ...taskdir.FaultInjector) (*taskdir.TaskDir, *taskdir.StartPermit, Plan, []byte) {
	t.Helper()
	s, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	require(t, err)
	t.Cleanup(func() { require(t, s.Close()) })
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	exe, err := os.Executable()
	require(t, err)
	exe, err = filepath.EvalSymlinks(exe)
	require(t, err)
	id, err := task.NewTaskID()
	require(t, err)
	brief := []byte("  literal '$HOME' $(inert) \"Ω\"\nsecond line\n\n")
	config := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	req := &task.TaskRecord{SchemaVersion: 1, RootID: s.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, RequestedConfig: config, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	meta := &task.MetaRecord{SchemaVersion: 1, RootID: s.RootID, TaskID: id, RequestedConfig: config, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("execution-test"))}, Containment: "fixture-only", Approval: "never", ProviderExecutable: exe, ProviderVersion: "fixture-v1", PublisherBuild: "test-build", PublisherVersion: "test", Predicate: task.FixturePredicateRef(), SupervisorConfig: task.SupervisorRef{ConfigPath: "/fake/pueue.yml", ConfigDigest: task.ComputeSHA256([]byte("pueue")), Endpoint: "/fake/socket", ObservedVersion: "fixture-v1"}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	plan := Plan{Executable: exe, Arguments: []string{"-test.run=^TestExecutionHelper$"}, Directory: cwd, Environment: append(os.Environ(), "DELEGATE_EXECUTION_HELPER="+mode), Predicate: task.FixturePredicateRef()}
	if configure != nil {
		configure(meta, &plan)
	}
	td, err := s.CreateTask(id, req, brief, meta)
	require(t, err)
	t.Cleanup(func() { require(t, td.Close()) })
	permit, err := td.PrepareStart(0)
	require(t, err)
	if len(faults) > 0 {
		s.SetFaultInjector(faults[0])
	}
	return td, permit, plan, brief
}

// The test executable is a finite real child. Self-exit prevents the Go test
// harness's PASS banner from becoming fake provider stdout.
func TestExecutionHelper(t *testing.T) {
	mode := os.Getenv("DELEGATE_EXECUTION_HELPER")
	if mode == "" {
		return
	}
	if err := helperOutput(mode); err != nil {
		os.Exit(31)
	}
	if mode == "nonzero" {
		os.Exit(7)
	}
	os.Exit(0)
}

func helperOutput(mode string) error {
	brief, err := io.ReadAll(io.LimitReader(os.Stdin, task.MaxBriefSize+1))
	if err != nil {
		return err
	}
	if len(brief) > task.MaxBriefSize {
		return errors.New("brief overflow")
	}
	if mode == "large" {
		if _, err = io.Copy(os.Stdout, strings.NewReader(strings.Repeat("Ωx", 400000))); err != nil {
			return err
		}
		_, err = io.Copy(os.Stderr, strings.NewReader(strings.Repeat("late-error-stream\n", 80000)))
		return err
	}
	_, err = os.Stdout.Write(brief)
	return err
}

type eventLog struct {
	mu    sync.Mutex
	names []string
}

func (e *eventLog) add(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.names = append(e.names, name)
}

func (e *eventLog) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.names...)
}

func eventIndex(t *testing.T, events []string, name string) int {
	t.Helper()
	for i, v := range events {
		if v == name {
			return i
		}
	}
	t.Fatalf("missing event %q: %v", name, events)
	return -1
}

func assertOrder(t *testing.T, events []string, before, after string) {
	t.Helper()
	if eventIndex(t, events, before) >= eventIndex(t, events, after) {
		t.Fatalf("expected %s before %s: %v", before, after, events)
	}
}

func assertSuccess(t *testing.T, r Result) {
	t.Helper()
	require(t, errors.Join(r.Error, r.CleanupError, r.ReceiptError, r.StopError))
	if r.Outcome == nil || r.Outcome.Verdict != task.VerdictCommitted {
		t.Fatalf("missing committed outcome: %+v", r)
	}
}

func payload(t *testing.T, td *taskdir.TaskDir, out *task.OutcomeRecord) []byte {
	t.Helper()
	r, err := td.OpenPayload(out)
	require(t, err)
	b, err := io.ReadAll(r)
	require(t, errors.Join(err, r.Close()))
	return b
}

func TestOwnedInvocationExactStdinAndNoRelaunch(t *testing.T) {
	td, permit, plan, brief := fixtureTask(t, "echo")
	events := &eventLog{}
	r := Run(td, permit, plan, Options{Hooks: Hooks{Event: events.add}})
	assertSuccess(t, r)
	if !bytes.Equal(payload(t, td, r.Outcome), brief) {
		t.Fatal("stdin/result bytes changed")
	}
	for _, pair := range [][2]string{{"timer-armed", "start-entry"}, {"start-entry", "started"}, {"started", "parent-fds-closed"}, {"wait-completed", "sealed"}, {"stdout-eof", "sealed"}, {"stderr-eof", "sealed"}, {"stdout-raw-closed", "sealed"}, {"stderr-raw-closed", "sealed"}, {"timer-disarmed", "sealed"}, {"sealed", "published"}} {
		assertOrder(t, events.snapshot(), pair[0], pair[1])
	}
	_, err := td.PrepareStart(0)
	if !errors.Is(err, task.ErrAlreadyStarted) {
		t.Fatalf("retry gained start authority: %v", err)
	}
	out, cleanup, err := td.Collect(plan.Predicate)
	require(t, errors.Join(cleanup, err))
	if !bytes.Equal(payload(t, td, out), brief) {
		t.Fatal("collection changed result")
	}
	count := 0
	for _, e := range events.snapshot() {
		if e == "start-entry" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Start entries=%d", count)
	}
}

func TestOwnedCaptureDrainsBothPipesBeyondBackpressure(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "large")
	r := Run(td, permit, plan, Options{})
	assertSuccess(t, r)
	if got := payload(t, td, r.Outcome); !bytes.Equal(got, []byte(strings.Repeat("Ωx", 400000))) {
		t.Fatalf("stdout not exact, got %d bytes", len(got))
	}
	// This is bounded fixture verification, not an unbounded production read.
	errBytes, err := os.ReadFile(filepath.Join(td.Dir, "raw", "stderr"))
	require(t, err)
	if !bytes.Equal(errBytes, []byte(strings.Repeat("late-error-stream\n", 80000))) {
		t.Fatal("stderr truncated or changed")
	}
}

func TestDefiniteStartFailureAndNonzeroExitAreSealedRefusals(t *testing.T) {
	for _, mode := range []string{"start-failed", "nonzero"} {
		t.Run(mode, func(t *testing.T) {
			td, permit, plan, _ := fixtureTask(t, mode)
			opts := Options{}
			if mode == "start-failed" {
				opts.Hooks.Start = func(*exec.Cmd) error { return os.ErrPermission }
			}
			r := Run(td, permit, plan, opts)
			require(t, errors.Join(r.Error, r.CleanupError, r.ReceiptError, r.StopError))
			if r.Outcome == nil || r.Outcome.Verdict != task.VerdictRejected {
				t.Fatalf("expected sealed refusal: %+v", r)
			}
			want := "provider_exit: 7"
			if mode == "start-failed" {
				want = "start_failed: " + os.ErrPermission.Error()
			}
			if string(payload(t, td, r.Outcome)) != want {
				t.Fatalf("wrong refusal: %q", payload(t, td, r.Outcome))
			}
			_, err := td.PrepareStart(0)
			if !errors.Is(err, task.ErrAlreadyStarted) {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestWaitAndCaptureInfrastructureFaultsNeverSeal(t *testing.T) {
	for _, mode := range []string{"wait", "wait-nonzero", "capture"} {
		t.Run(mode, func(t *testing.T) {
			helperMode := "echo"
			if mode == "wait-nonzero" {
				helperMode = "nonzero"
			}
			td, permit, plan, _ := fixtureTask(t, helperMode)
			injected := errors.New("injected infrastructure fault")
			opts := infrastructureFaultOptions(mode, injected)
			r := Run(td, permit, plan, opts)
			if !errors.Is(r.Error, injected) || r.Outcome != nil {
				t.Fatalf("fault manufactured publication: %+v", r)
			}
			inspection, err := td.Inspect()
			require(t, err)
			if inspection.SealExists || inspection.OutcomeExists {
				t.Fatal("infrastructure failure sealed or published")
			}
			_, err = td.PrepareStart(0)
			if !errors.Is(err, task.ErrAlreadyStarted) {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func infrastructureFaultOptions(mode string, injected error) Options {
	if mode == "wait" || mode == "wait-nonzero" {
		return Options{Hooks: Hooks{Wait: func(cmd *exec.Cmd) error {
			return errors.Join(cmd.Wait(), injected)
		}}}
	}
	return Options{Hooks: Hooks{Capture: func(name string, w io.Writer, r io.Reader) error {
		_, err := io.Copy(w, r)
		if name == "stdout" {
			return errors.Join(err, injected)
		}
		return err
	}}}
}

func TestExpiredBudgetWithoutStopCapabilityIsReported(t *testing.T) {
	clock := newManualClock()
	expired := make(chan struct{})
	opts := Options{Hooks: Hooks{Event: func(name string) {
		if name == "deadline-observed" {
			close(expired)
		}
	}}}
	b, _ := armBudget(clock, time.Second, opts)
	clock.timer.ch <- clock.now.Add(time.Second)
	<-expired
	b.complete(opts)
	if b.await() == nil {
		t.Fatal("missing enforcement capability was silently ignored")
	}
}

func TestFailedCaptureStillDrainsFiniteChild(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "large")
	injected := errors.New("capture stopped after prefix")
	events := &eventLog{}
	opts := Options{Hooks: Hooks{Event: events.add, Capture: func(name string, w io.Writer, r io.Reader) error {
		if name == "stderr" {
			_, err := io.Copy(w, r)
			return err
		}
		_, err := io.CopyN(w, r, 8)
		return errors.Join(err, injected)
	}}}
	r := Run(td, permit, plan, opts)
	if !errors.Is(r.Error, injected) || r.Outcome != nil {
		t.Fatalf("capture fault was lost: %+v", r)
	}
	assertOrder(t, events.snapshot(), "stdout-drained-after-error", "completion-observed")
	eventIndex(t, events.snapshot(), "wait-completed")
	inspection, err := td.Inspect()
	require(t, err)
	if inspection.SealExists {
		t.Fatal("failed capture was sealed")
	}
}

type failStartedReceipt struct {
	taskdir.BaseFaultInjector
	failure error
}

func (f *failStartedReceipt) OnStageWrite(path string, data []byte) (int, error) {
	if filepath.Base(path) == "provider.started.json" {
		return 0, f.failure
	}
	return len(data), nil
}

func TestStartedReceiptFailureRetainsProcessAndPublicationOwnership(t *testing.T) {
	injected := errors.New("started receipt write failed")
	td, permit, plan, brief := fixtureTask(t, "echo", &failStartedReceipt{failure: injected})
	r := Run(td, permit, plan, Options{})
	require(t, errors.Join(r.Error, r.CleanupError, r.StopError))
	if !errors.Is(r.ReceiptError, injected) {
		t.Fatalf("lost receipt failure: %+v", r)
	}
	if r.Outcome == nil || !bytes.Equal(payload(t, td, r.Outcome), brief) {
		t.Fatal("receipt error abandoned natural capture/publication")
	}
	inspection, err := td.Inspect()
	require(t, err)
	if inspection.StartedExists || !inspection.SealExists || !inspection.OutcomeExists {
		t.Fatalf("wrong independent evidence: %+v", inspection)
	}
}

type (
	manualClock struct {
		now   time.Time
		timer *manualTimer
	}
	manualTimer struct{ ch chan time.Time }
)

func (c manualClock) Now() time.Time               { return c.now }
func (c manualClock) NewTimer(time.Duration) Timer { return c.timer }
func (t *manualTimer) C() <-chan time.Time         { return t.ch }
func (*manualTimer) Stop() bool                    { return true }

type stopperFunc func(context.Context, time.Time) error

func (f stopperFunc) PrepareBudget(d time.Time) (BudgetRequest, error) {
	return func(ctx context.Context) error { return f(ctx, d) }, nil
}

func newManualClock() manualClock {
	return manualClock{now: time.Now(), timer: &manualTimer{ch: make(chan time.Time, 1)}}
}

func TestBudgetCompletionAndExpiryOrder(t *testing.T) {
	for _, expireFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(expireFirst), func(t *testing.T) {
			clock := newManualClock()
			called := make(chan struct{}, 1)
			opts := Options{Stopper: stopperFunc(func(context.Context, time.Time) error { called <- struct{}{}; return nil })}
			b, _ := armBudget(clock, time.Second, opts)
			if expireFirst {
				clock.timer.ch <- clock.now.Add(time.Second)
				select {
				case <-called:
				case <-time.After(5 * time.Second):
					t.Fatal("expiry was not observed")
				}
			}
			b.complete(opts)
			if !expireFirst {
				clock.timer.ch <- clock.now.Add(time.Second)
			}
			require(t, b.await())
			select {
			case <-called:
				t.Fatal("stop after observed completion")
			default:
			}
		})
	}
}

func TestDeadlineObservedWhileStartOperationIsBlocked(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	clock := newManualClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{}, 1)
	events := &eventLog{}
	opts := Options{Clock: clock, Stopper: stopperFunc(func(context.Context, time.Time) error { stopped <- struct{}{}; return nil }), Hooks: Hooks{Event: events.add, Start: func(cmd *exec.Cmd) error { close(entered); <-release; return cmd.Start() }}}
	result := make(chan Result, 1)
	go func() { result <- Run(td, permit, plan, opts) }()
	<-entered
	clock.timer.ch <- clock.now.Add(time.Minute)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("blocked Start disabled deadline observation")
	}
	close(release)
	r := <-result
	assertSuccess(t, r)
	assertOrder(t, events.snapshot(), "deadline-observed", "started")
}

func TestPublicationDoesNotWaitForDelayedStopReply(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	clock := newManualClock()
	stopStarted := make(chan struct{})
	published := make(chan struct{})
	late := errors.New("stop observation ended with reply unknown")
	opts := Options{Clock: clock, Stopper: stopperFunc(func(ctx context.Context, _ time.Time) error {
		close(stopStarted)
		<-ctx.Done()
		<-published
		return late
	}), Hooks: Hooks{Start: func(cmd *exec.Cmd) error {
		clock.timer.ch <- clock.now.Add(time.Minute)
		<-stopStarted
		return cmd.Start()
	}, Event: func(name string) {
		if name == "published" {
			close(published)
		}
	}}}
	r := Run(td, permit, plan, opts)
	require(t, errors.Join(r.Error, r.CleanupError, r.ReceiptError))
	if r.Outcome == nil || r.Outcome.Verdict != task.VerdictCommitted || !errors.Is(r.StopError, late) {
		t.Fatalf("stop observation hid publication: %+v", r)
	}
}
