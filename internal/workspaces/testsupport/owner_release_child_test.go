package testsupport

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"golang.org/x/sys/unix"
)

var ownerReleaseProbeID = strings.Repeat("4", 32)

var errProbeUnlock = errors.New("probe lifetime owner unlock failed")

// probeUnlockFault fails the first unlock of one lifetime-owner lock.
type probeUnlockFault struct {
	taskdir.BaseFaultInjector
	path  string
	fired bool
}

func (f *probeUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (f *probeUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *probeUnlockFault) OnFlock(path string, operation int) error {
	if path == f.path && operation == unix.LOCK_UN && !f.fired {
		f.fired = true
		return errProbeUnlock
	}
	return nil
}

// persistProbeOperation records the probe operation in fixture.
func persistProbeOperation(t *testing.T, fixture *RecoveryFixture) {
	t.Helper()
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: ownerReleaseProbeID, SHA256: task.ComputeSHA256([]byte("probe"))}
	request := RecoveryOperationRequest(t, fixture, ownerReleaseProbeID, workspace.OperationVerify, subject)
	request.Arguments = []string{"/usr/bin/true"}
	PersistSelectionOperation(t, fixture, request)
}

// persistProbeFixture establishes a fixture with one recorded operation under
// the parent base and closes every parent handle.
func persistProbeFixture(t *testing.T, base *OwnerReleaseBase) string {
	t.Helper()
	fixture := NewRecoveryFixtureIn(t, base.Dir)
	persistProbeOperation(t, fixture)
	root := fixture.Store.Root
	if err := fixture.CloseHandles(); err != nil {
		t.Fatal(err)
	}
	return root
}

// holdChildOwner acquires the operation lifetime owner in the child and
// deliberately leaves the lease, operation and store open: like a failed
// release, only the child's exit ends the lock.
func holdChildOwner(t *testing.T, base *OwnerReleaseBase) {
	t.Helper()
	store, err := taskdir.OpenStore(RecoveryFixtureRoot(base.Dir))
	if err != nil {
		t.Fatal(err)
	}
	operation, err := workspace.LoadOperation(t.Context(), store, ownerReleaseProbeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operation.TryAcquireLifetimeOwner(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func requireBaseRemoved(t *testing.T, base *OwnerReleaseBase) {
	t.Helper()
	if _, err := os.Stat(base.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent fixture base after proven closure: %v", err)
	}
}

func TestOwnerReleaseChildProvesHeldOwnerClosed(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	if base.Child() {
		holdChildOwner(t, base)
		return
	}
	root := persistProbeFixture(t, base)
	if err := base.runChild(t.Context(), t.Name(), root, []string{ownerReleaseProbeID}); err != nil {
		t.Fatal(err)
	}
	requireBaseRemoved(t, base)
}

func TestOwnerReleaseChildFailureStillProbesAndCleans(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	if base.Child() {
		holdChildOwner(t, base)
		t.Error("intentional child assertion failure")
		return
	}
	root := persistProbeFixture(t, base)
	err := base.runChild(t.Context(), t.Name(), root, []string{ownerReleaseProbeID})
	if err == nil || !strings.Contains(err.Error(), "intentional child assertion failure") || strings.Contains(err.Error(), "retained") {
		t.Fatalf("failed child = %v; want its failure and a proven closure", err)
	}
	requireBaseRemoved(t, base)
}

func TestOwnerReleaseChildTimeoutStillProbesAndCleans(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	if base.Child() {
		holdChildOwner(t, base)
		// Only the child's own -test.timeout ends this wait.
		<-t.Context().Done()
		return
	}
	base.timeout = time.Second
	root := persistProbeFixture(t, base)
	err := base.runChild(t.Context(), t.Name(), root, []string{ownerReleaseProbeID})
	if err == nil || !strings.Contains(err.Error(), "test timed out") || strings.Contains(err.Error(), "retained") {
		t.Fatalf("timed out child = %v; want its timeout and a proven closure", err)
	}
	requireBaseRemoved(t, base)
}

// holdParentOwner holds the probe operation's lifetime owner in the parent on
// its own store. The base tracks the record before any acquisition, so its
// final settlement releases it even after an early failed assertion.
func holdParentOwner(t *testing.T, base *OwnerReleaseBase, root string) {
	t.Helper()
	store, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	held := &heldOwner{store: store}
	base.held = append(base.held, held)
	if held.operation, err = workspace.LoadOperation(t.Context(), store, ownerReleaseProbeID); err != nil {
		t.Fatal(err)
	}
	if held.lease, err = held.operation.TryAcquireLifetimeOwner(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func requireBaseRetained(t *testing.T, base *OwnerReleaseBase, err error, reason string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "fixture retained at "+base.Dir+": "+reason) {
		t.Fatalf("settlement = %v; want the base retained for %q", err, reason)
	}
	if _, statErr := os.Stat(base.Dir); statErr != nil {
		t.Fatalf("unproven fixture was not retained: %v", statErr)
	}
}

func TestOwnerReleaseChildRetainsUnprovenFixture(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	if base.Child() {
		ReopenRecoveryFixture(t, RecoveryFixtureRoot(base.Dir))
		return
	}
	root := persistProbeFixture(t, base)
	holdParentOwner(t, base, root)
	err := base.runChild(t.Context(), t.Name(), root, []string{ownerReleaseProbeID})
	requireBaseRetained(t, base, err, "operation "+ownerReleaseProbeID+" lifetime owner")
	// The final settlement releases the parent owner, then proves and removes.
	if err = base.settle(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	requireBaseRemoved(t, base)
}

// TestOwnerReleaseProbeReleaseFaultRetainsHandles fails the probe's own owner
// release once: the base keeps that exact lease, operation and store until
// its final settlement releases them, proves the owner free and removes.
func TestOwnerReleaseProbeReleaseFaultRetainsHandles(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	if base.Child() {
		holdChildOwner(t, base)
		return
	}
	root := persistProbeFixture(t, base)
	fault := &probeUnlockFault{path: filepath.Join(root, "workspace-operations", ownerReleaseProbeID, ".lifetime-owner.lock")}
	base.probeFault = fault
	err := base.runChild(t.Context(), t.Name(), root, []string{ownerReleaseProbeID})
	if !fault.fired || !errors.Is(err, errProbeUnlock) {
		t.Fatalf("probe release fault = %v (fired %t)", err, fault.fired)
	}
	requireBaseRetained(t, base, err, "operation "+ownerReleaseProbeID+": release operation lifetime owner")
	if len(base.held) != 1 || base.held[0].lease == nil || base.held[0].operation == nil || base.held[0].store == nil {
		t.Fatalf("probe handles after a failed release = %+v; want the exact handles held", base.held)
	}
	probe := base.held[0]
	if err = base.settle(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if probe.lease != nil || probe.operation != nil || probe.store != nil {
		t.Fatalf("final settlement left probe handles open: %+v", probe)
	}
	requireBaseRemoved(t, base)
}

// TestOwnerReleaseBaseRetainsUnclosedParentFixture fails the parent fixture's
// close before any child launch: the base keeps the fixture handle and is
// retained, and is removed only after that handle closes.
func TestOwnerReleaseBaseRetainsUnclosedParentFixture(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	fixture := NewRecoveryFixtureIn(t, base.Dir)
	base.fixtures = append(base.fixtures, fixture)
	persistProbeOperation(t, fixture)
	operation, err := workspace.LoadOperation(t.Context(), fixture.Store, ownerReleaseProbeID)
	if err != nil {
		t.Fatal(err)
	}
	held := &heldOwner{operation: operation}
	t.Cleanup(func() {
		if closeErr := held.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if held.lease, err = operation.TryAcquireLifetimeOwner(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The live lease keeps the fixture store's maintenance lock.
	closeErr := fixture.CloseHandles()
	if closeErr == nil || fixture.Store == nil || fixture.Resource != nil {
		t.Fatalf("fixture close with a live lease = %v (store %v, resource %v); want the store kept", closeErr, fixture.Store, fixture.Resource)
	}
	err = base.settle(t.Context(), true)
	requireBaseRetained(t, base, err, "close parent fixture: "+closeErr.Error())
	if err = held.close(); err != nil {
		t.Fatal(err)
	}
	if err = base.settle(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if fixture.Store != nil {
		t.Fatal("final settlement left the fixture store open")
	}
	requireBaseRemoved(t, base)
}

// TestOwnerReleaseChildRetainsStartedChildWithoutWaitState settles a started
// child whose exit state is unknown: the base is retained before any probe,
// with the run error and output.
func TestOwnerReleaseChildRetainsStartedChildWithoutWaitState(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	base.launched, base.root, base.operationIDs = true, persistProbeFixture(t, base), []string{ownerReleaseProbeID}
	waitErr := errors.New("wait state unavailable")
	err := base.settleChild(t.Context(), childRun{output: []byte("child output"), err: waitErr}, t.Name())
	if !errors.Is(err, waitErr) || !strings.Contains(err.Error(), "child output") {
		t.Fatalf("unknown child closure = %v; want the run error and output", err)
	}
	requireBaseRetained(t, base, err, "started child has no exact wait state")
	if err = base.settle(t.Context(), true); err == nil {
		t.Fatal("final settlement removed a base after an unknown child closure")
	}
	// No child ran in this control, so its closure is known after all.
	base.unknownChild = false
}

// TestOwnerReleaseChildStartFailureProbesAndCleans settles a child that
// never started: no process can hold the base, so it is probed and removed.
func TestOwnerReleaseChildStartFailureProbesAndCleans(t *testing.T) {
	base := NewOwnerReleaseBase(t)
	base.launched, base.root, base.operationIDs = true, persistProbeFixture(t, base), []string{ownerReleaseProbeID}
	err := base.settleChild(t.Context(), childRun{err: exec.ErrNotFound, closed: true}, t.Name())
	if !errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "retained") {
		t.Fatalf("unstarted child = %v; want its start failure and a proven closure", err)
	}
	requireBaseRemoved(t, base)
}

// TestOwnerReleaseBaseUsesCanonicalPlatformTempDir selects an owned TMPDIR
// reached through a symlink and requires the base at its physical path.
func TestOwnerReleaseBaseUsesCanonicalPlatformTempDir(t *testing.T) {
	physical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "tmp-alias")
	if err = os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	base := NewOwnerReleaseBase(t)
	if base.Child() || filepath.Dir(base.Dir) != physical || !strings.HasPrefix(filepath.Base(base.Dir), "dla-owner-") {
		t.Fatalf("owner release base = %q (child %t); want a new base directly under %q", base.Dir, base.Child(), physical)
	}
	if info, statErr := os.Lstat(base.Dir); statErr != nil || !info.IsDir() {
		t.Fatalf("owner release base %q = %v, %v", base.Dir, info, statErr)
	}
}

func TestOwnerReleaseChildRunPatternIsExact(t *testing.T) {
	if got, want := childRunPattern("TestA/case_[x]+"), `^TestA$/^case_\[x\]\+$`; got != want {
		t.Fatalf("pattern = %q, want %q", got, want)
	}
	err := childOutcome("TestA", []byte("--- PASS: TestAB (0.00s)\nPASS\n"), nil)
	if err == nil {
		t.Fatal("another test's PASS line was accepted")
	}
	if err = childOutcome("TestA", []byte("testing: warning: no tests to run\nPASS\n"), nil); err == nil {
		t.Fatal("a run that selected no test was accepted")
	}
}
