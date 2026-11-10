package control

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

func TestInvocationDiagnosticSinkPreservesCauseWithoutChangingClient(t *testing.T) {
	want := errors.New("owned cleanup fault")
	var observed []error
	client := &Client{options: Options{ReportDiagnostic: func(err error) { observed = append(observed, err) }}}
	client.ReportDiagnostic(nil)
	client.ReportDiagnostic(want)
	if len(observed) != 1 || !errors.Is(observed[0], want) {
		t.Fatalf("invocation diagnostics = %v", observed)
	}
	// Telemetry neither allocates work ownership nor changes lifecycle state.
	if client.workOwner != nil || client.binding.ConfigPath != "" {
		t.Fatal("diagnostic reporting acquired lifecycle authority")
	}
}

func TestRetainedDiagnosticChoosesFirstAvailableSinkOnce(t *testing.T) {
	want := errors.New("one retained cleanup event")
	var chosen, other []error
	noSink := transport.NewPending(nil, nil)
	first := transport.NewPending(nil, func(err error) { chosen = append(chosen, err) })
	second := transport.NewPending(nil, func(err error) { other = append(other, err) })
	reportRetained([]*Pending{nil, noSink, first, second, first}, want)
	if len(chosen) != 1 || !errors.Is(chosen[0], want) || len(other) != 0 {
		t.Fatalf("cleanup event crossed invocation sinks: chosen=%v other=%v", chosen, other)
	}
}

func TestRetainedCleanupUsesOneCausalSinkAfterEveryCommandSettles(t *testing.T) {
	want := errors.New("deferred cleanup fault")
	reported, unrelated := make(chan error, 2), make(chan error, 2)
	first := transport.NewPending(nil, nil)
	second := transport.NewPending(nil, func(err error) { reported <- err })
	third := transport.NewPending(nil, func(err error) { unrelated <- err })
	cleanupDone := make(chan struct{})
	var cleanupCount atomic.Int32
	t.Cleanup(func() {
		for _, pending := range []*Pending{first, second, third} {
			transport.CompletePending(pending, CommandResult{})
		}
		select {
		case <-cleanupDone:
		case <-time.After(time.Second):
			t.Error("deferred cleanup did not finish")
		}
	})
	cause := errors.Join(&InFlightError{Pending: first}, &InFlightError{Pending: second}, &InFlightError{Pending: third}, &InFlightError{Pending: second})
	if err := retainCleanupUntil(func() error {
		cleanupCount.Add(1)
		close(cleanupDone)
		return want
	}, cause); err != nil {
		t.Fatal(err)
	}
	transport.CompletePending(first, CommandResult{})
	transport.CompletePending(second, CommandResult{})
	if cleanupCount.Load() != 0 {
		t.Fatal("cleanup ran while its third command still owned work")
	}
	transport.CompletePending(third, CommandResult{})
	select {
	case got := <-reported:
		if !errors.Is(got, want) || cleanupCount.Load() != 1 {
			t.Fatalf("cleanup cause/count = %v/%d", got, cleanupCount.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("original invocation lost deferred cleanup telemetry")
	}
	select {
	case got := <-reported:
		t.Fatalf("duplicate causal report: %v", got)
	case got := <-unrelated:
		t.Fatalf("cleanup also reported to another invocation: %v", got)
	default:
	}
}

func TestStartedCommandsRetainTheirInvocationDiagnosticSink(t *testing.T) {
	for _, independent := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "control-owner"}[independent], func(t *testing.T) {
			reported := make(chan error, 1)
			report := func(err error) { reported <- err }
			command := exec.Command(finiteExecutable(t, "exit 0"))
			command.Dir = filepath.Dir(command.Path)
			var pending *Pending
			if independent {
				pending = startControlOwnedCapturedWithConsume(t.Context(), os.Args[0], command, strings.Repeat("d", 32), nil, "", nil, report)
			} else {
				pending = startOwnedCaptured(command, "diagnostic-test", nil, "", report)
			}
			t.Cleanup(func() {
				if !awaitPendingNaturally(pending, controlOwnerTestStartupTimeout) {
					t.Error("finite command remained owned during diagnostic fixture cleanup")
				}
				<-pending.Done()
			})
			awaitControlOwnerTest(t, pending)
			result := waitPending(t, pending)
			if result.Err != nil || !result.Started || result.ExitCode != 0 {
				t.Fatalf("finite command did not close normally: %+v", result)
			}
			want := errors.New("late command cleanup")
			reportRetained([]*Pending{nil, pending}, want)
			select {
			case got := <-reported:
				if !errors.Is(got, want) {
					t.Fatalf("retained command cause = %v", got)
				}
			default:
				t.Fatal("command constructor discarded invocation reporter")
			}
		})
	}
}

func TestBootstrapCleanupLateFaultUsesPendingInvocationReporter(t *testing.T) {
	lock := closedDiagnosticBootstrapLock(t)
	reported := make(chan error, 2)
	pending := transport.NewPending(nil, func(err error) { reported <- err })
	t.Cleanup(func() { transport.CompletePending(pending, CommandResult{}) })
	resultErr := error(&InFlightError{Pending: pending})
	finishPrivateBootstrap(&lock, &resultErr)
	var owner *BootstrapCleanupError
	if !errors.As(resultErr, &owner) || lock != nil {
		t.Fatal("cleanup did not transfer lock ownership")
	}
	transport.CompletePending(pending, CommandResult{})
	awaitBootstrapCleanup(t, owner)
	select {
	case got := <-reported:
		if !errors.Is(got, os.ErrClosed) || !errors.Is(owner.Err(), os.ErrClosed) {
			t.Fatalf("release fault lost its concrete cause: report=%v owner=%v", got, owner.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("late native release fault bypassed invocation reporter")
	}
	for range 2 {
		if !errors.Is(owner.Close(), os.ErrClosed) {
			t.Fatal("cleanup observation discarded release fault")
		}
	}
	select {
	case got := <-reported:
		t.Fatalf("observing historical cleanup fault reported it twice: %v", got)
	default:
	}
}

// A finite child owns its default stderr; this never redirects the test
// process's global logger or races another invocation's output.
func TestDiagnosticFallbackAndSynchronousCleanupDoNotDuplicate(t *testing.T) {
	const childFlag = "DELEGATE_TEST_DIAGNOSTIC_FALLBACK"
	const marker = "diagnostic-default-fallback-fixture"
	if os.Getenv(childFlag) == "1" {
		reportRetained([]*Pending{nil}, errors.New(marker))
		reportRetained([]*Pending{nil}, nil)
		lock := closedDiagnosticBootstrapLock(t)
		cause := errors.New("synchronous command fault")
		resultErr := cause
		finishPrivateBootstrap(&lock, &resultErr)
		if lock != nil || !errors.Is(resultErr, cause) || !errors.Is(resultErr, os.ErrClosed) {
			t.Fatalf("synchronous close fault was not joined: %v", resultErr)
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestDiagnosticFallbackAndSynchronousCleanupDoNotDuplicate$")
	command.Env = append(os.Environ(), childFlag+"=1")
	result := runOwnedCommand(command, "", nil)
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("diagnostic child failed: %v stderr=%s", result.Err, result.Stderr)
	}
	if got := string(result.Stderr); strings.Count(got, marker) != 1 || strings.Contains(got, "retained supervisor lock cleanup failed") {
		t.Fatalf("fallback absent or synchronous fault also logged: %q", got)
	}
}

func closedDiagnosticBootstrapLock(t *testing.T) *bootstrapLock {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "closed-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return &bootstrapLock{file: file}
}

func TestDaemonWaitCompletesBeforeBlockedInvocationReporter(t *testing.T) {
	root := t.TempDir()
	command := exec.Command(finiteExecutable(t, "exit 23"))
	publishedDone := make(chan (<-chan struct{}), 1)
	reported := make(chan error, 1)
	callbackDone, releaseCallback := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCallback) })
		select {
		case <-callbackDone:
		case <-time.After(time.Second):
			t.Error("finite daemon diagnostic callback did not finish")
		}
	})
	report := func(err error) {
		defer close(callbackDone)
		done := <-publishedDone
		select {
		case <-done:
			reported <- err
		default:
			reported <- errors.New("reporter ran before natural Wait was published")
		}
		<-releaseCallback
	}
	done, err := guardedDaemonStart(t.Context(), root, filepath.Join(root, "config"), command, allowDaemonStartForTest{}, report)
	if err != nil || done == nil {
		// No command owns a callback on a failed Start.
		close(callbackDone)
		t.Fatalf("finite daemon did not start: %v", err)
	}
	publishedDone <- done
	select {
	case got := <-reported:
		var exitErr *exec.ExitError
		if !errors.As(got, &exitErr) || exitErr.ExitCode() != 23 {
			t.Fatalf("daemon Wait/report ordering or cause changed: %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finite daemon did not report its Wait result")
	}
	select {
	case <-done:
	default:
		t.Fatal("blocked diagnostic sink retained daemon Wait ownership")
	}
}
