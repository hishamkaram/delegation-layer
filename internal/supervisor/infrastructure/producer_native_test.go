package infrastructure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	producerChildArgument = "--producer-component-child"
	producerReadyMessage  = "producer-component-ready"
)

type producerOutput struct {
	mu        sync.Mutex
	data      bytes.Buffer
	ready     chan struct{}
	announced bool
}

func (o *producerOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.data.Write(data)
	if !o.announced && bytes.Contains(o.data.Bytes(), []byte(producerReadyMessage)) {
		o.announced = true
		close(o.ready)
	}
	return n, err
}

func (o *producerOutput) String() string { o.mu.Lock(); defer o.mu.Unlock(); return o.data.String() }

type producerNativeFixture struct {
	parent      string
	store       *taskdir.Store
	host        *producerTestHost
	request     EstablishmentRequest
	raw         []string
	input       *os.File
	stop        *os.File
	output      *producerOutput
	attempts    []*StartupAttempt
	command     *exec.Cmd
	parentLease *os.File
	waits       atomic.Int32
	retain      bool
}

func newProducerNativeFixture(t *testing.T) *producerNativeFixture {
	t.Helper()
	requireComponentEntryTestHost(t)
	f := &producerNativeFixture{raw: os.Environ(), output: &producerOutput{ready: make(chan struct{})}}
	t.Cleanup(func() { f.cleanup(t) })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	f.parent, err = os.MkdirTemp(home, ".ip-")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(f.parent)
	if err != nil {
		t.Fatal(err)
	}
	f.parent = parent
	f.store, err = taskdir.InitStore(filepath.Join(parent, "state"))
	if err != nil {
		f.retain = true
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := Artifacts{Runner: runner}
	f.host = &producerTestHost{artifacts: artifacts}
	f.request = EstablishmentRequest{Context: LaunchContext{Protocol: RecordProtocolVersion, Root: f.store.Root, RootID: f.store.RootID, OS: runtime.GOOS, ControlEnvironment: []string{"DELEGATION_LAYER_PROCESS_SCOPE=pg:11111111111111111111111111111111", "DELEGATE_INFRASTRUCTURE_ORIGIN=foreign configuration", "HOME=/selected/configuration"}, MaintenanceEnvironment: []string{}, SupervisorExecutable: filepath.Join(f.store.Root, "pueue")}, Deadline: time.Now().Add(time.Minute)}
	f.input, f.stop, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The private adapter preserves production fixed argv as the helper's payload.
// It accepts only this test image through the existing componentEntryHost. No
// managed-image eligibility, daemon preparation or product authority is claimed.
func (f *producerNativeFixture) native(t *testing.T, afterStart func()) producerNative {
	t.Helper()
	return f.nativeForChild(t, afterStart, "TestProducerNativeChild")
}

func (f *producerNativeFixture) nativeForChild(t *testing.T, afterStart func(), child string) producerNative {
	t.Helper()
	return producerNative{start: func(command *exec.Cmd) error {
		f.command = command
		f.parentLease = command.ExtraFiles[0]
		if !slices.Equal(command.Env, f.raw) {
			return errors.New("native birth environment differs from checked raw snapshot")
		}
		command.Args = append([]string{command.Path, "-test.run=^" + child + "$", "-test.timeout=20s", "--", producerChildArgument}, command.Args[1:]...)
		command.Stdin = f.input
		command.Stdout = f.output
		command.Stderr = f.output
		startErr := command.Start()
		closeErr := f.input.Close()
		f.input = nil
		if startErr == nil && afterStart != nil {
			afterStart()
		}
		return errors.Join(startErr, closeErr)
	}, wait: func(command *exec.Cmd) error { f.waits.Add(1); return command.Wait() }, observe: func(pid int) (lifetime.ProcessRef, error) {
		if _, err := f.parentLease.Stat(); !errors.Is(err, os.ErrClosed) {
			return lifetime.ProcessRef{}, errors.Join(errors.New("parent duplicate remained open during child observation"), err)
		}
		lease, acquired, err := TryAcquireLease(t.Context(), filepath.Join(f.store.Root, instanceLeaseName))
		if lease != nil {
			err = errors.Join(err, lease.Close())
		}
		if err != nil || acquired {
			return lifetime.ProcessRef{}, errors.Join(errors.New("child did not alone retain inherited lease"), err)
		}
		return lifetime.ObserveStartedProcess(pid)
	}}
}

func (f *producerNativeFixture) cleanup(t *testing.T) {
	t.Helper()
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if f.stop != nil {
		if err := f.stop.Close(); err != nil {
			t.Errorf("close child input: %v", err)
			f.retain = true
		}
		f.stop = nil
	}
	if f.input != nil {
		if err := f.input.Close(); err != nil {
			t.Errorf("close unused child input: %v", err)
			f.retain = true
		}
		f.input = nil
	}
	f.closeAttempts(t)
	if f.command != nil && f.command.Process != nil && f.waits.Load() != 1 {
		t.Errorf("native child has no sole joined Wait; retain %s", f.parent)
		f.retain = true
	}
	f.closeStore(t, cleanupCtx)
	if !f.retain && f.parent != "" {
		if err := os.RemoveAll(f.parent); err != nil {
			t.Error(err)
		}
	}
}

func (f *producerNativeFixture) closeStore(t *testing.T, cleanupCtx context.Context) {
	t.Helper()
	if f.store != nil {
		lease, acquired, err := TryAcquireLease(cleanupCtx, filepath.Join(f.store.Root, instanceLeaseName))
		if lease != nil {
			if closeErr := lease.Close(); closeErr != nil {
				t.Errorf("close cleanup lease: %v", closeErr)
				f.retain = true
			}
		}
		if err != nil || !acquired {
			t.Errorf("instance lease not free; retain %s: %v", f.parent, err)
			f.retain = true
		}
		if err = f.store.Close(); err != nil {
			t.Errorf("close producer store; retain %s: %v", f.parent, err)
			f.retain = true
		}
	}
}

func (f *producerNativeFixture) closeAttempts(t *testing.T) {
	t.Helper()
	for _, attempt := range f.attempts {
		if done := attempt.NativeCompletion(); done != nil {
			select {
			case <-done:
				if waitErr, complete := attempt.NativeWaitError(); !complete || waitErr != nil {
					t.Errorf("native child completion failed; retain %s: %v", f.parent, waitErr)
					f.retain = true
				}
			case <-time.After(10 * time.Second):
				t.Errorf("unresolved producer child; retain %s", f.parent)
				f.retain = true
			}
		}
		if err := attempt.Close(); err != nil {
			t.Errorf("close producer resources; retain %s: %v", f.parent, err)
			f.retain = true
		}
	}
}

func (f *producerNativeFixture) ready(t *testing.T, attempt *StartupAttempt) {
	t.Helper()
	select {
	case <-f.output.ready:
	case <-attempt.NativeCompletion():
		err, _ := attempt.NativeWaitError()
		t.Fatalf("component child exited before publication: %v\n%s", err, f.output.String())
	case <-time.After(10 * time.Second):
		f.retain = true
		t.Fatal("component child did not publish before fixture deadline")
	}
}

func TestProducerNativeLifecycle(t *testing.T) {
	for _, mode := range []string{"handoff", "post-start-root-error", "observer-canceled"} {
		if !t.Run(mode, func(t *testing.T) { exerciseProducerNative(t, mode) }) {
			t.FailNow()
		}
	}
}

func exerciseProducerNative(t *testing.T, mode string) {
	t.Helper()
	f := newProducerNativeFixture(t)
	originalID := f.store.RootID
	ctxStart, cancelStart := context.WithCancel(t.Context())
	defer cancelStart()
	var after func()
	switch mode {
	case "post-start-root-error":
		after = func() { f.store.RootID = "post-start-invalid" }
	case "observer-canceled":
		after = cancelStart
	}
	attempt, err := producerInvocation(t).establishOutside(ctxStart, f.store, f.request, f.host, f.native(t, after))
	if attempt != nil {
		f.attempts = append(f.attempts, attempt)
	}
	f.store.RootID = originalID
	if attempt == nil || attempt.Reference().Sequence != 1 || attempt.NativeCompletion() == nil {
		t.Fatalf("native birth lost acceptance/Wait: %v", err)
	}
	if (err != nil) != (mode == "post-start-root-error") {
		t.Fatalf("native birth outcome: %v", err)
	}
	f.ready(t, attempt)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ready, observeErr := attempt.Observe(ctx)
	if observeErr != nil || !ready {
		t.Fatalf("native publication: ready=%t err=%v", ready, observeErr)
	}
	publication, err := attempt.journal.readPublication(ctx, attempt.Reference())
	if err != nil || publication.Startup != attempt.Reference() {
		t.Fatalf("native publication reference: %v", err)
	}
	if err = attempt.Close(); err != nil {
		t.Fatal(err)
	}
	f.assertLiveJoin(t, attempt)
}

func (f *producerNativeFixture) assertLiveJoin(t *testing.T, attempt *StartupAttempt) {
	t.Helper()
	select {
	case <-attempt.NativeCompletion():
		t.Fatal("closing parent resources ended resident ownership")
	default:
	}
	joined, joinErr := producerInvocation(t).EstablishOutside(t.Context(), f.store, f.request, f.host)
	if joined != nil {
		f.attempts = append(f.attempts, joined)
	}
	var contact *OwnerContactRequiredError
	if !errors.As(joinErr, &contact) || contact.Startup != attempt.Reference() || joined != nil || f.host.calls != 1 {
		t.Fatalf("live matching owner did not require contact: prepare=%d err=%v", f.host.calls, joinErr)
	}
	lease, acquired, leaseErr := TryAcquireLease(t.Context(), filepath.Join(f.store.Root, instanceLeaseName))
	if lease != nil {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if leaseErr != nil || acquired {
		t.Fatalf("child failed to retain inherited OFD: acquired=%t err=%v", acquired, leaseErr)
	}
}

func TestProducerNativeChild(t *testing.T) { runProducerNativeChild(t, false) }

func TestProducerNativePendingChild(t *testing.T) { runProducerNativeChild(t, true) }

func runProducerNativeChild(t *testing.T, pending bool) {
	t.Helper()
	index := slices.Index(os.Args, producerChildArgument)
	if index < 0 {
		return
	}
	session, sessionErr := unix.Getsid(0)
	if sessionErr != nil || session != os.Getpid() || unix.Getpgrp() != os.Getpid() {
		t.Fatalf("component child lacks independent native session: %v", sessionErr)
	}
	invocation, handled, err := parseOwnerInvocation(os.Args[index+1:])
	if err != nil || !handled {
		t.Fatalf("fixed child arguments: %v", err)
	}
	if pending {
		var permit [1]byte
		if _, readErr := io.ReadFull(os.Stdin, permit[:]); errors.Is(readErr, io.EOF) {
			return
		} else if readErr != nil {
			t.Fatal(readErr)
		}
	}
	entry, err := consumeOwnerInvocation(t.Context(), invocation, &componentEntryHost{})
	ownComponentEntry(t, entry)
	if err != nil || entry == nil {
		t.Fatalf("native component entry: %v", err)
	}
	if _, err = fmt.Fprintln(os.Stdout, producerReadyMessage); err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	if err = entry.Close(); err != nil {
		t.Fatal(err)
	}
}

// Capture actual native/environment placement before target preparation, without
// creating containment resources or changing the parent's environment.
func TestProducerContainedInvocationRefusesBeforePreparation(t *testing.T) {
	if slices.Contains(os.Args, "--producer-contained-test") {
		j, lease := journalFixture(t)
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		request := EstablishmentRequest{Context: contextForJournal(t, j), Deadline: time.Now().Add(time.Minute)}
		host := &producerTestHost{}
		source, err := CaptureInvocationSource(t.Context())
		var attempt *StartupAttempt
		if err == nil {
			attempt, err = source.EstablishOutside(t.Context(), j.store, request, host)
		}
		ownProducerAttempt(t, attempt)
		if !errors.Is(err, lifetime.ErrTaskContained) || host.calls != 0 || attempt != nil {
			t.Fatalf("contained caller reached preparation: calls=%d err=%v", host.calls, err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestProducerContainedInvocationRefusesBeforePreparation$", "-test.timeout=20s", "--", "--producer-contained-test")
	command.Env = append(os.Environ(), "DELEGATION_LAYER_PROCESS_SCOPE=pg:11111111111111111111111111111111")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err = command.Run(); err != nil {
		t.Fatalf("contained boundary child failed: %v\n%s", err, output.String())
	}
}
