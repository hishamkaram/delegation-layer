package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// executionTargetFixture is a private root whose fake pair bound and
// published one generation. Both executables are removed afterwards, so any
// subprocess launched by the observer would fail.
type executionTargetFixture struct {
	stateRoot  string
	configPath string
	endpoint   string
	pidPath    string
}

func newExecutionTargetFixture(t *testing.T) executionTargetFixture {
	t.Helper()
	pair := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, pair.stateRoot, pair.clientPath, pair.daemonPath)
	binding := client.Binding()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, pair.clientPath)
	removePrivateTestPath(t, pair.daemonPath)
	endpoint, err := privateEndpointSocketPath(binding.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return executionTargetFixture{
		stateRoot:  pair.stateRoot,
		configPath: binding.ConfigPath,
		endpoint:   endpoint,
		pidPath:    filepath.Join(pair.stateRoot, privateSupervisorDirectory, "run", "pueue.pid"),
	}
}

func (fixture executionTargetFixture) recordPID(t *testing.T, pid int) {
	t.Helper()
	if err := os.WriteFile(fixture.pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// listen serves the exact endpoint from the test process until cleanup. The
// kernel completes connections in the backlog, so no accept loop is needed.
func (fixture executionTargetFixture) listen(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("unix", fixture.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}

// leaveRefusedSocket leaves a socket inode at the endpoint with no listener.
func (fixture executionTargetFixture) leaveRefusedSocket(t *testing.T) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: fixture.endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
}

// commandCountingOptions counts every supervisor command the observer would
// start through the client's canonical command path.
func commandCountingOptions(commands *atomic.Int64) Options {
	return Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, Observer: func(CommandEvent) { commands.Add(1) }}
}

func snapshotExecutionTargetTree(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%v %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 == 0 {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			value += ":" + string(data)
		}
		entries[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertExecutionTargetTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := snapshotExecutionTargetTree(t, root)
	if len(after) != len(before) {
		t.Fatalf("observer changed the state tree: before=%d entries after=%d entries", len(before), len(after))
	}
	for path, want := range before {
		if got, ok := after[path]; !ok || got != want {
			t.Fatalf("observer changed %s: before=%q after=%q", path, want, got)
		}
	}
}

func assertExecutionTargetUnknown(t *testing.T, target ExecutionTarget, err error, cause error, detail string) {
	t.Helper()
	if !errors.Is(err, ErrExecutionTargetUnverified) {
		t.Fatalf("observation error = %v, want ErrExecutionTargetUnverified", err)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("observation error = %v, want cause %v", err, cause)
	}
	if detail != "" && !strings.Contains(err.Error(), detail) {
		t.Fatalf("observation error = %v, want detail %q", err, detail)
	}
	if target != (ExecutionTarget{}) || target.State() != ExecutionTargetUnknown {
		t.Fatalf("unverified observation returned target %+v", target)
	}
}

func assertExecutionTargetAbsent(t *testing.T, target ExecutionTarget, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if target.State() != ExecutionTargetAbsent || target.PID() != 0 || target.StartToken() != "" {
		t.Fatalf("target = %+v, want absent", target)
	}
}

func TestObservePrivateExecutionTargetColdRootIsAbsent(t *testing.T) {
	_, stateRoot := newPrivateSupervisorTestRoots(t)
	before := snapshotExecutionTargetTree(t, stateRoot)
	var commands atomic.Int64
	target, err := ObservePrivateExecutionTarget(context.Background(), stateRoot, commandCountingOptions(&commands))
	assertExecutionTargetAbsent(t, target, err)
	assertExecutionTargetTreeUnchanged(t, stateRoot, before)
	missingRoot := filepath.Join(canonicalTemp(t), "missing")
	target, err = ObservePrivateExecutionTarget(context.Background(), missingRoot, commandCountingOptions(&commands))
	assertExecutionTargetAbsent(t, target, err)
	if _, statErr := os.Lstat(missingRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("observer created the missing state root: %v", statErr)
	}
	if commands.Load() != 0 {
		t.Fatalf("observer started %d supervisor commands", commands.Load())
	}
}

func TestObservePrivateExecutionTargetRejectsInvalidCalls(t *testing.T) {
	target, err := ObservePrivateExecutionTarget(context.Background(), "relative/state", Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
	assertExecutionTargetUnknown(t, target, err, ErrConfiguration, "")
	var nilContext context.Context
	target, err = ObservePrivateExecutionTarget(nilContext, canonicalTemp(t), Options{DaemonStartGuard: allowDaemonStartForTest{}})
	assertExecutionTargetUnknown(t, target, err, context.Canceled, "")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	target, err = ObservePrivateExecutionTarget(cancelled, canonicalTemp(t), Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
	assertExecutionTargetUnknown(t, target, err, context.Canceled, "")
}

// Existing private state that cannot prove a complete generation is never
// reported as absent.
func TestObservePrivateExecutionTargetKeepsUnprovenStateUnknown(t *testing.T) {
	t.Run("directory without publication", func(t *testing.T) {
		_, stateRoot := newPrivateSupervisorTestRoots(t)
		makePrivateTestDirectory(t, filepath.Join(stateRoot, privateSupervisorDirectory))
		target, err := ObservePrivateExecutionTarget(context.Background(), stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
		assertExecutionTargetUnknown(t, target, err, ErrBinding, "not published")
	})
	t.Run("malformed publication", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		path := privateSupervisorBindingPath(fixture.stateRoot)
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotExecutionTargetTree(t, fixture.stateRoot)
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
		assertExecutionTargetUnknown(t, target, err, ErrBinding, "")
		assertExecutionTargetTreeUnchanged(t, fixture.stateRoot, before)
	})
	t.Run("pending bootstrap", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		if err := markPrivateBootstrapPending(fixture.stateRoot); err != nil {
			t.Fatal(err)
		}
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
		assertExecutionTargetUnknown(t, target, err, ErrBinding, "transition is pending")
	})
	t.Run("config changed after publication", func(t *testing.T) {
		fixture := newExecutionTargetFixture(t)
		appendPrivateSupervisorFile(t, fixture.configPath, "\n# changed\n")
		target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
		assertExecutionTargetUnknown(t, target, err, ErrBinding, "")
	})
}

func TestObservePrivateExecutionTargetStoppedGeneration(t *testing.T) {
	fixture := newExecutionTargetFixture(t)
	removePrivateTestPath(t, fixture.pidPath)
	before := snapshotExecutionTargetTree(t, fixture.stateRoot)
	var commands atomic.Int64
	target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, commandCountingOptions(&commands))
	assertExecutionTargetAbsent(t, target, err)
	assertExecutionTargetTreeUnchanged(t, fixture.stateRoot, before)

	fixture.leaveRefusedSocket(t)
	before = snapshotExecutionTargetTree(t, fixture.stateRoot)
	target, err = ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, commandCountingOptions(&commands))
	assertExecutionTargetUnknown(t, target, err, ErrBinding, "without a recorded PID")
	assertExecutionTargetTreeUnchanged(t, fixture.stateRoot, before)
	if commands.Load() != 0 {
		t.Fatalf("observer started %d supervisor commands", commands.Load())
	}
}

func TestVerifyPrivateExecutionPeer(t *testing.T) {
	identity := privateProcessIdentity{PID: 41, StartToken: "linux:7"}
	uid := 501
	tests := []struct {
		name   string
		before privateProcessIdentity
		after  privateProcessIdentity
		peer   privateEndpointPeer
		detail string
	}{
		{name: "verified", before: identity, after: identity, peer: privateEndpointPeer{PID: 41, UID: uid}},
		{name: "absent before", before: privateProcessIdentity{PID: 41}, after: identity, peer: privateEndpointPeer{PID: 41, UID: uid}, detail: "absent"},
		{name: "absent after", before: identity, after: privateProcessIdentity{PID: 41}, peer: privateEndpointPeer{PID: 41, UID: uid}, detail: "absent"},
		{name: "restarted", before: identity, after: privateProcessIdentity{PID: 41, StartToken: "linux:8"}, peer: privateEndpointPeer{PID: 41, UID: uid}, detail: "changed"},
		{name: "wrong peer", before: identity, after: identity, peer: privateEndpointPeer{PID: 42, UID: uid}, detail: "not the recorded process"},
		{name: "wrong user", before: identity, after: identity, peer: privateEndpointPeer{PID: 41, UID: uid + 1}, detail: "another user"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyPrivateExecutionPeer(test.before, test.after, test.peer, uid)
			if test.detail == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrBinding) || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("verify error = %v, want ErrBinding with %q", err, test.detail)
			}
		})
	}
}
