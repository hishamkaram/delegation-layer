package verifiedexec

// A fleet of finite owned handshake children.
//
// Owner: readerFleet owns every child it started and every pipe endpoint it
// created. Writers: start, release and the cleanup registered when each child
// starts. Invariant: every endpoint created before Start is closed on every
// failure path, and every started child has its input closed and is Waited
// exactly once. No signal is sent: closing stdin ends the helper's handshake,
// and the helper is finite on its own. Whether a view may be released after
// that Wait is decided by the test, not by this fixture; it proves nothing
// about the canonical running state of a real task.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fleetReader is one started child and its single-use reap.
type fleetReader struct {
	cmd    *exec.Cmd
	stdin  *os.File
	reaped bool
	err    error
	used   childResources
	known  bool
}

// reap closes the child's handshake input and Waits once. Later calls return
// the first result without touching the process again.
func (r *fleetReader) reap() error {
	if r.reaped {
		return r.err
	}
	r.reaped = true
	closeErr := r.stdin.Close()
	waitErr := r.cmd.Wait()
	r.used, r.known = resourcesOf(r.cmd.ProcessState)
	r.err = errors.Join(closeErr, waitErr)
	return r.err
}

type readerFleet struct {
	t       *testing.T
	readers []*fleetReader
	// startFault and releaseFault inject a failure at a child index; nil in
	// the real run.
	startFault, releaseFault func(index int) error
	// childEndFault injects a failure when the parent closes its copy of the
	// child's input pipe right after a successful Start.
	childEndFault func(index int) error
}

func newReaderFleet(t *testing.T) *readerFleet { return &readerFleet{t: t} }

// start launches one bound command with stdout on stdout and an input pipe the
// fleet owns outright. Both pipe endpoints are closed on every path before
// Start succeeds; from the moment Start succeeds, the test cleanup owns reaping
// this child.
func (f *readerFleet) start(command *Command, stdout *os.File) error {
	if f.startFault != nil {
		if err := f.startFault(len(f.readers)); err != nil {
			return err
		}
	}
	cmd := command.Cmd
	if cmd.Stdin != nil || cmd.Stdout != nil {
		return errStdioConfigured
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout = inRead, stdout
	index := len(f.readers)
	if err = cmd.Start(); err != nil {
		return errors.Join(err, inRead.Close(), inWrite.Close())
	}
	// The child exists from here on: own it before any fallible close, so an
	// early return still closes its input and Waits for it exactly once.
	reader := &fleetReader{cmd: cmd, stdin: inWrite}
	f.readers = append(f.readers, reader)
	f.t.Cleanup(func() {
		if !reader.reaped {
			if reapErr := reader.reap(); reapErr != nil {
				f.t.Errorf("cleanup of an unreleased reader: %v", reapErr)
			}
		}
	})
	closeErr := inRead.Close()
	if f.childEndFault != nil {
		closeErr = errors.Join(closeErr, f.childEndFault(index))
	}
	return closeErr
}

// startAll launches every command on one shared append-only stdout file. The
// children keep their inherited copy; the parent closes its own.
func (f *readerFleet) startAll(commands []*Command, outPath string) (resultErr error) {
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, out.Close()) }()
	for i, command := range commands {
		if err = f.start(command, out); err != nil {
			return fmt.Errorf("start reader %d: %w", i, err)
		}
	}
	return nil
}

// release answers GO to every started child and then reaps every one of them,
// whatever failed along the way.
func (f *readerFleet) release() error {
	var failure error
	for i, reader := range f.readers {
		if f.releaseFault != nil {
			if err := f.releaseFault(i); err != nil {
				failure = errors.Join(failure, err)
				continue
			}
		}
		if _, err := io.WriteString(reader.stdin, "GO\n"); err != nil {
			failure = errors.Join(failure, fmt.Errorf("release reader %d: %w", i, err))
		}
	}
	for i, reader := range f.readers {
		if err := reader.reap(); err != nil {
			failure = errors.Join(failure, fmt.Errorf("reader %d: %w", i, err))
		}
	}
	return failure
}

var errInjectedFleetFault = errors.New("injected fleet fault")

func faultAt(index int) func(int) error {
	return func(got int) error {
		if got == index {
			return errInjectedFleetFault
		}
		return nil
	}
}

// bindFleet binds n helper commands, one owner each, into the rig's pool.
func bindFleet(rig *poolRig, entry, digest, prefix string, n int) []*Command {
	commands := make([]*Command, n)
	for i := range commands {
		commands[i] = legacyBinder(rig.t, rig.dir)(fmt.Sprintf("%s-%d", prefix, i), CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg, "handshake"}})
	}
	return commands
}

// TestReaderFleetReapsEveryStartedChildOnFailure drives each failing path in a
// subtest that returns without reaping, so only the cleanup registered at each
// child's start can wait for it. After the subtest ends, every started child
// must be waited and the parent's descriptors must be back at the baseline.
func TestReaderFleetReapsEveryStartedChildOnFailure(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := selfNative(t, filepath.Join(rig.dir, "fleet"))
	// A first cycle lets the runtime open its lazy descriptors, so the baseline
	// below is stable.
	t.Run("warm-up", func(t *testing.T) { runFleetScenario(t, rig, entry, digest, "warm", 2, nil, nil) })
	baseline := openDescriptorsOrFail(t)

	for _, tc := range []struct {
		name         string
		children     int
		startFault   func(int) error
		releaseFault func(int) error
		wantErr      bool
	}{
		{"start-failure-after-two-started", 2, faultAt(2), nil, true},
		{"setup-failure-after-all-started", 3, nil, nil, false},
		{"release-failure-on-one-child", 3, nil, faultAt(1), true},
	} {
		var fleet *readerFleet
		t.Run(tc.name, func(t *testing.T) {
			fleet = runFleetScenario(t, rig, entry, digest, tc.name, tc.children, tc.startFault, tc.releaseFault)
		})
		if len(fleet.readers) != tc.children {
			t.Fatalf("%s: %d children started, want %d", tc.name, len(fleet.readers), tc.children)
		}
		for i, reader := range fleet.readers {
			if !reader.reaped || reader.cmd.ProcessState == nil || !reader.cmd.ProcessState.Exited() {
				t.Fatalf("%s: started child %d was not waited", tc.name, i)
			}
		}
		if after := openDescriptorsOrFail(t); after != baseline {
			t.Fatalf("%s: %d descriptors open, %d at the baseline", tc.name, after, baseline)
		}
		rig.rec.note("fleet-"+tc.name, "%d started children waited, descriptors back at the baseline %d", tc.children, baseline)
	}
}

func openDescriptorsOrFail(t *testing.T) int {
	t.Helper()
	open, err := openDescriptors()
	if err != nil {
		t.Fatalf("setup blocked: count descriptors: %v", err)
	}
	return open
}

// runFleetScenario binds and starts children, fails in the way the scenario
// names (an injected fault, or by returning early), and returns without
// reaping what the scenario did not release.
func runFleetScenario(t *testing.T, rig *poolRig, entry, digest, scenario string, children int, startFault, releaseFault func(int) error) *readerFleet {
	t.Helper()
	fleet := newReaderFleet(t)
	fleet.startFault, fleet.releaseFault = startFault, releaseFault
	bound := children
	if startFault != nil {
		bound++ // one more than will start, so the fault is what stops the fleet
	}
	commands := bindFleet(rig, entry, digest, scenario, bound)
	err := fleet.startAll(commands, filepath.Join(rig.dir, "fleet-"+scenario+".out"))
	switch {
	case startFault != nil:
		if !errors.Is(err, errInjectedFleetFault) {
			t.Fatalf("start gave %v; want the injected fault", err)
		}
	case err != nil:
		t.Fatal(err)
	case releaseFault != nil:
		if err = fleet.release(); !errors.Is(err, errInjectedFleetFault) {
			t.Fatalf("release gave %v; want the injected fault", err)
		}
	case scenario == "warm":
		if err = fleet.release(); err != nil {
			t.Fatal(err)
		}
	}
	return fleet
}

// TestFleetAndHandshakeRefusePreconfiguredStdioWithoutLeakingOrStarting is the
// deterministic no-child regression for the setup leak: a command whose stdout
// is already configured must be refused before any pipe is created, no child
// may exist, and the descriptor count must not move.
func TestFleetAndHandshakeRefusePreconfiguredStdioWithoutLeakingOrStarting(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := selfNative(t, filepath.Join(rig.dir, "stdio"))
	command := legacyBinder(rig.t, rig.dir)("a", CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg, "handshake"}})
	sink, err := os.CreateTemp(rig.dir, "sink-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := sink.Close(); closeErr != nil {
			t.Errorf("close sink: %v", closeErr)
		}
	})
	baseline := openDescriptorsOrFail(t)

	command.Cmd.Stdout = sink
	if _, err = startHandshake(t, command); !errors.Is(err, errStdioConfigured) {
		t.Fatalf("startHandshake with stdout configured gave %v", err)
	}
	if command.Cmd.Process != nil {
		t.Fatal("a child was started despite the refusal")
	}
	if after := openDescriptorsOrFail(t); after != baseline {
		t.Fatalf("startHandshake leaked: %d descriptors, baseline %d", after, baseline)
	}

	fleet := newReaderFleet(t)
	command.Cmd.Stdout = sink
	if err = fleet.start(command, sink); !errors.Is(err, errStdioConfigured) || len(fleet.readers) != 0 {
		t.Fatalf("fleet.start with stdout configured gave %v with %d readers", err, len(fleet.readers))
	}
	if after := openDescriptorsOrFail(t); after != baseline {
		t.Fatalf("fleet.start leaked: %d descriptors, baseline %d", after, baseline)
	}

	// A real Start failure after the pipes exist must close all of them too.
	broken := legacyBinder(rig.t, rig.dir)("b", CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg, "handshake"}})
	broken.Cmd.Path = filepath.Join(rig.dir, "does-not-exist")
	if err = fleet.start(broken, sink); err == nil || len(fleet.readers) != 0 {
		t.Fatalf("fleet.start of a missing program gave %v with %d readers", err, len(fleet.readers))
	}
	if after := openDescriptorsOrFail(t); after != baseline {
		t.Fatalf("a failed Start leaked: %d descriptors, baseline %d", after, baseline)
	}
	rig.rec.note("no-child-setup-failure", "refused before any pipe; descriptors stayed at %d", baseline)
}

// TestHandshakeChildAndWorkerAreReapedOnEarlyExit covers the other two fixture
// paths that start a child: a handshake child and a worker process. Each
// subtest returns without releasing or waiting, as an early t.Fatal would.
func TestHandshakeChildAndWorkerAreReapedOnEarlyExit(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := selfNative(t, filepath.Join(rig.dir, "early"))
	command := legacyBinder(rig.t, rig.dir)("early", CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg, "handshake"}})
	var child *liveChild
	var worker *workerProcess
	t.Run("handshake-child", func(t *testing.T) {
		var err error
		if child, err = startHandshake(t, command); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("worker", func(t *testing.T) {
		worker = startWorker(t, modeObserve, rig.pool.root, rig.id, "early-worker")
	})
	if !child.finished || child.cmd.ProcessState == nil || !child.cmd.ProcessState.Exited() {
		t.Fatal("the handshake child was left unwaited by an early exit")
	}
	if !worker.reaped || worker.cmd.ProcessState == nil || !worker.cmd.ProcessState.Exited() {
		t.Fatal("the worker was left unwaited by an early exit")
	}
	rig.rec.note("early-exit", "handshake child and worker were waited by their registered cleanups")
}

// TestFleetRegistersWaitOwnershipBeforeTheFalliblePipeClose injects a failure
// into the post-Start close of the child's input endpoint. The child is
// started, so it must already be owned: Waited exactly once by the registered
// cleanup, with every endpoint closed.
func TestFleetRegistersWaitOwnershipBeforeTheFalliblePipeClose(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := selfNative(t, filepath.Join(rig.dir, "close-fault"))
	baseline := openDescriptorsOrFail(t)
	var fleet *readerFleet
	var startErr error
	t.Run("start", func(t *testing.T) {
		fleet = newReaderFleet(t)
		fleet.childEndFault = faultAt(0)
		command := legacyBinder(rig.t, rig.dir)("a", CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg, "handshake"}})
		startErr = fleet.start(command, os.Stderr)
	})
	if !errors.Is(startErr, errInjectedFleetFault) {
		t.Fatalf("start gave %v; want the injected close fault", startErr)
	}
	if len(fleet.readers) != 1 {
		t.Fatalf("%d readers registered; a started child must be owned before the fallible close", len(fleet.readers))
	}
	reader := fleet.readers[0]
	if !reader.reaped || reader.cmd.ProcessState == nil || !reader.cmd.ProcessState.Exited() {
		t.Fatal("the started child was not Waited by its registered cleanup")
	}
	if after := openDescriptorsOrFail(t); after != baseline {
		t.Fatalf("%d descriptors open, %d at the baseline", after, baseline)
	}
}
