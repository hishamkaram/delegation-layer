//go:build linux || darwin

package control

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
)

func nativeTestProcessToken(t *testing.T, pid int) string {
	t.Helper()
	observation, err := observeNativePrivateProcess(pid)
	if err != nil || !observation.Present {
		t.Fatalf("native process %d observation: present=%t err=%v", pid, observation.Present, err)
	}
	return observation.StartToken
}

// The test process records its own PID and serves the exact endpoint, so the
// kernel-reported peer credentials match the recorded process incarnation.
func TestObservePrivateExecutionTargetNativeVerifiesEndpointPeer(t *testing.T) {
	fixture := newExecutionTargetFixture(t)
	fixture.recordPID(t, os.Getpid())
	fixture.listen(t)
	before := snapshotExecutionTargetTree(t, fixture.stateRoot)
	var commands atomic.Int64
	target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, commandCountingOptions(&commands))
	if err != nil {
		t.Fatal(err)
	}
	if target.State() != ExecutionTargetRunning || target.PID() != os.Getpid() {
		t.Fatalf("target = %+v, want running test process %d", target, os.Getpid())
	}
	if want := nativeTestProcessToken(t, os.Getpid()); target.StartToken() != want {
		t.Fatalf("start token = %q, want %q", target.StartToken(), want)
	}
	assertExecutionTargetTreeUnchanged(t, fixture.stateRoot, before)
	if commands.Load() != 0 {
		t.Fatalf("observer started %d supervisor commands", commands.Load())
	}
}

func TestObservePrivateExecutionTargetNativeRejectsUnboundPeer(t *testing.T) {
	t.Run("recorded PID is another live process", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		fixture.recordPID(t, os.Getppid())
		fixture.listen(t)
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
		assertExecutionTargetUnknown(t, target, err, ErrBinding, "not the recorded process")
	})
	t.Run("start token changes across the connection", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		fixture.recordPID(t, os.Getpid())
		fixture.listen(t)
		var observations atomic.Int64
		options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, processObserver: func(int) (privateProcessObservation, error) {
			return privateProcessObservation{Present: true, StartToken: "fixture:" + strconv.FormatInt(observations.Add(1), 10)}, nil
		}}
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, options)
		assertExecutionTargetUnknown(t, target, err, ErrBinding, "identity changed")
	})
	t.Run("recorded process is absent", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		fixture.recordPID(t, os.Getpid())
		fixture.listen(t)
		options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, processObserver: func(int) (privateProcessObservation, error) {
			return privateProcessObservation{}, nil
		}}
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, options)
		assertExecutionTargetUnknown(t, target, err, ErrBinding, "absent")
	})
}

func TestObservePrivateExecutionTargetNativeDeadEndpointIsUnknown(t *testing.T) {
	t.Run("refused socket", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		fixture.recordPID(t, os.Getpid())
		fixture.leaveRefusedSocket(t)
		before := snapshotExecutionTargetTree(t, fixture.stateRoot)
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
		assertExecutionTargetUnknown(t, target, err, syscall.ECONNREFUSED, "")
		assertExecutionTargetTreeUnchanged(t, fixture.stateRoot, before)
	})
	t.Run("missing socket", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		fixture.recordPID(t, os.Getpid())
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
		assertExecutionTargetUnknown(t, target, err, os.ErrNotExist, "")
	})
}

// TestPrivateSupervisorNativeExecutionTarget proves the observer against the
// real Pueue daemon bound by the test-owned fixture, which stops it at cleanup.
func TestPrivateSupervisorNativeExecutionTarget(t *testing.T) {
	fixture := newNativePrivateSupervisorFixture(t)
	client, err := fixture.bind(t)
	if err != nil {
		t.Fatal(err)
	}
	process, present, err := client.privateDaemonProcessIdentity()
	if err != nil || !present {
		t.Fatalf("native daemon identity: present=%t err=%v", present, err)
	}
	var commands atomic.Int64
	target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, commandCountingOptions(&commands))
	if err != nil {
		t.Fatal(err)
	}
	if target.State() != ExecutionTargetRunning || target.PID() != process.PID || target.StartToken() != process.StartToken {
		t.Fatalf("target = %+v, want running daemon %+v", target, process)
	}
	if commands.Load() != 0 {
		t.Fatalf("observer started %d supervisor commands", commands.Load())
	}
}
