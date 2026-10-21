package testsupport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// ownerReleaseChildBase carries the parent-owned fixture base into the child
// test process; its presence selects the child half of a test.
const ownerReleaseChildBase = "DELEGATE_TEST_OWNER_RELEASE_BASE"

// ownerReleaseChildTimeout bounds one child run through the test binary's
// own -test.timeout, so the child ends itself without parent signals.
const ownerReleaseChildTimeout = 3 * time.Minute

// ownerReleaseCleanupTimeout bounds the final settlement, which runs after
// the test context is cancelled.
const ownerReleaseCleanupTimeout = 30 * time.Second

// OwnerReleaseBase is a fixture directory owned by a parent test and used by
// one child process running that same test. A failed lifetime-owner release
// returns before its locked descriptor closes, so only the process exit ends
// that lock. The base is the only authority that removes the directory: it
// removes it once settle proves the closure, and otherwise retains it with
// the reason. The parent handles it tracks stay open until their own release
// or close succeeds.
type OwnerReleaseBase struct {
	Dir     string
	child   bool
	timeout time.Duration
	// launched records that a child was started for this base; root and
	// operationIDs name the lifetime owners probed after it.
	launched     bool
	root         string
	operationIDs []string
	// unknownChild records a started child without an exact wait state.
	unknownChild bool
	// fixtures and held are parent handles under the base.
	fixtures []*RecoveryFixture
	held     []*heldOwner
	// probeFault is installed on the probe store; only focused controls set it.
	probeFault taskdir.FaultInjector
	removed    bool
}

// heldOwner is one lifetime owner with the exact handles that must stay open
// until each is released or closed successfully. A nil store belongs to
// another owner.
type heldOwner struct {
	store     *taskdir.Store
	operation *workspace.Operation
	lease     *taskdir.LifetimeOwnerLease
}

// closeOperation releases the lease and closes the operation, clearing each
// handle only after it succeeds.
func (h *heldOwner) closeOperation() error {
	if h.lease != nil {
		if err := h.lease.Release(); err != nil {
			return fmt.Errorf("release operation lifetime owner: %w", err)
		}
		h.lease = nil
	}
	if h.operation != nil {
		if err := h.operation.Close(); err != nil {
			return fmt.Errorf("close operation: %w", err)
		}
		h.operation = nil
	}
	return nil
}

// close closes the operation handles and then the owned store.
func (h *heldOwner) close() error {
	if err := h.closeOperation(); err != nil {
		return err
	}
	if h.store != nil {
		if err := h.store.Close(); err != nil {
			return fmt.Errorf("close store: %w", err)
		}
		h.store = nil
	}
	return nil
}

// NewOwnerReleaseBase returns the parent's base inside the child process, and
// otherwise a new parent-owned base in the platform temporary directory, at
// its physical canonical path. Its cleanup is the final settlement.
func NewOwnerReleaseBase(t *testing.T) *OwnerReleaseBase {
	t.Helper()
	if dir := os.Getenv(ownerReleaseChildBase); dir != "" {
		return &OwnerReleaseBase{Dir: dir, child: true}
	}
	dir, err := os.MkdirTemp("", "dla-owner-")
	if err != nil {
		t.Fatal(err)
	}
	base := &OwnerReleaseBase{Dir: dir, timeout: ownerReleaseChildTimeout}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), ownerReleaseCleanupTimeout)
		defer cancel()
		if settleErr := base.settle(ctx, true); settleErr != nil {
			t.Error(settleErr)
		}
	})
	base.Dir = supervisorfixture.CanonicalPath(t, dir)
	return base
}

// Child reports whether this process is the child that runs the test body.
func (b *OwnerReleaseBase) Child() bool { return b.child }

// RunChild reruns exactly the calling test in a child process of the current
// test binary, waits for it, probes the lifetime owner of every operation
// recorded under root plus operationIDs, and removes the base when all are
// free. The parent must close its own fixture handles before calling it.
func (b *OwnerReleaseBase) RunChild(t *testing.T, root string, operationIDs ...string) {
	t.Helper()
	if err := b.runChild(t.Context(), t.Name(), root, operationIDs); err != nil {
		t.Fatal(err)
	}
}

func (b *OwnerReleaseBase) runChild(ctx context.Context, name, root string, operationIDs []string) error {
	if b.child || b.launched {
		return errors.New("owner release child must be launched once by its parent")
	}
	b.launched, b.root, b.operationIDs = true, root, operationIDs
	return b.settleChild(ctx, b.run(name), name)
}

// childRun is the parent's knowledge of one child run.
type childRun struct {
	output []byte
	err    error
	// closed holds when no child started, or when Wait returned the started
	// child's exit state. Only then may the parent probe and remove the base.
	closed bool
}

// settleChild reports the child's outcome and settles the base after it.
func (b *OwnerReleaseBase) settleChild(ctx context.Context, run childRun, name string) error {
	b.unknownChild = !run.closed
	return errors.Join(childOutcome(name, run.output, run.err), b.settle(ctx, false))
}

// settle removes the base once its closure is proven, and otherwise retains
// it and reports why. Final settlement first closes the tracked parent
// handles; earlier settlement requires the parent fixtures already closed.
func (b *OwnerReleaseBase) settle(ctx context.Context, final bool) error {
	if b.removed {
		return nil
	}
	if err := b.closureProof(ctx, final); err != nil {
		return fmt.Errorf("fixture retained at %s: %w", b.Dir, err)
	}
	if err := os.RemoveAll(b.Dir); err != nil {
		return err
	}
	b.removed = true
	return nil
}

// closureProof requires a known child closure, closed parent handles and,
// once a child ran, every probed lifetime owner free.
func (b *OwnerReleaseBase) closureProof(ctx context.Context, final bool) error {
	if b.unknownChild {
		return errors.New("started child has no exact wait state")
	}
	if final {
		if err := b.closeParentHandles(); err != nil {
			return err
		}
	} else if b.parentFixturesOpen() {
		return errors.New("parent fixture handles are open")
	}
	if !b.launched {
		return nil
	}
	return b.probeLifetimeOwners(ctx)
}

func (b *OwnerReleaseBase) closeParentHandles() error {
	for _, held := range b.held {
		if err := held.close(); err != nil {
			return err
		}
	}
	for _, fixture := range b.fixtures {
		if err := fixture.CloseHandles(); err != nil {
			return fmt.Errorf("close parent fixture: %w", err)
		}
	}
	return nil
}

func (b *OwnerReleaseBase) parentFixturesOpen() bool {
	return slices.ContainsFunc(b.fixtures, func(fixture *RecoveryFixture) bool {
		return fixture.Store != nil || fixture.Resource != nil
	})
}

// OwnerReleaseRecoveryFixture runs the calling test's fault half in a child
// process. The parent persists a recovery fixture under its base with
// persist, closes its handles, runs the child and returns nil after proving
// the lifetime owners free. The child gets the reopened fixture. The base
// tracks the parent fixture, so a failed close before launch retains it.
func OwnerReleaseRecoveryFixture(t *testing.T, persist func(*testing.T, *RecoveryFixture), operationIDs ...string) *RecoveryFixture {
	t.Helper()
	base := NewOwnerReleaseBase(t)
	if base.Child() {
		return ReopenRecoveryFixture(t, RecoveryFixtureRoot(base.Dir))
	}
	fixture := NewRecoveryFixtureIn(t, base.Dir)
	base.fixtures = append(base.fixtures, fixture)
	persist(t, fixture)
	root := fixture.Store.Root
	if err := fixture.CloseHandles(); err != nil {
		t.Fatal(err)
	}
	base.RunChild(t, root, operationIDs...)
	return nil
}

// run starts the test binary on the exact test name and waits for it.
func (b *OwnerReleaseBase) run(name string) childRun {
	executable, err := os.Executable()
	if err != nil {
		return childRun{err: err, closed: true}
	}
	command := exec.Command(executable, "-test.run="+childRunPattern(name), "-test.count=1", "-test.v", "-test.timeout="+b.timeout.String())
	command.Env = append(os.Environ(), ownerReleaseChildBase+"="+b.Dir)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Start(); err != nil {
		return childRun{output: output.Bytes(), err: err, closed: true}
	}
	err = command.Wait()
	return childRun{output: output.Bytes(), err: err, closed: command.ProcessState != nil}
}

// childRunPattern anchors each element of a test name. Subtest names must
// not contain a slash of their own.
func childRunPattern(name string) string {
	elements := strings.Split(name, "/")
	for i, element := range elements {
		elements[i] = "^" + regexp.QuoteMeta(element) + "$"
	}
	return strings.Join(elements, "/")
}

// childOutcome accepts only a clean exit that ran and passed the exact test.
func childOutcome(name string, output []byte, runErr error) error {
	passed := bytes.Contains(output, []byte("--- PASS: "+name+" ("))
	if runErr == nil && passed && !bytes.Contains(output, []byte("--- FAIL")) && !bytes.Contains(output, []byte("--- SKIP")) {
		return nil
	}
	return errors.Join(fmt.Errorf("child test %s did not pass:\n%s", name, output), runErr)
}

// probeLifetimeOwners acquires and releases the lifetime owner of each
// operation that a workspace under the base root records, and of
// operationIDs, on its own store. After any failure the base holds the
// probe's handles exactly as left, for the final settlement to close.
func (b *OwnerReleaseBase) probeLifetimeOwners(ctx context.Context) error {
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, b.root, predicate.Default())
	if err != nil {
		return err
	}
	if b.probeFault != nil {
		store.SetFaultInjector(b.probeFault)
	}
	probe := &heldOwner{store: store}
	if err = b.probeOwners(ctx, probe); err == nil {
		err = probe.close()
	}
	if err != nil {
		b.held = append(b.held, probe)
	}
	return err
}

func (b *OwnerReleaseBase) probeOwners(ctx context.Context, probe *heldOwner) error {
	ids, err := recordedOperationIDs(ctx, probe.store)
	if err != nil {
		return err
	}
	for _, id := range b.operationIDs {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return errors.New("no operation lifetime owner to probe")
	}
	for _, id := range ids {
		if probe.operation, err = workspace.LoadOperation(ctx, probe.store, id); err != nil {
			return fmt.Errorf("load operation %s: %w", id, err)
		}
		if probe.lease, err = probe.operation.TryAcquireLifetimeOwner(ctx); err != nil {
			return fmt.Errorf("operation %s lifetime owner: %w", id, err)
		}
		if err = probe.closeOperation(); err != nil {
			return fmt.Errorf("operation %s: %w", id, err)
		}
	}
	return nil
}

func recordedOperationIDs(ctx context.Context, store *taskdir.Store) ([]string, error) {
	workspaceIDs, err := workspace.ResourceIDs(ctx, store)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, workspaceID := range workspaceIDs {
		resource, loadErr := workspace.LoadResource(ctx, store, workspaceID)
		if loadErr != nil {
			return nil, fmt.Errorf("load workspace %s: %w", workspaceID, loadErr)
		}
		recorded, listErr := resource.OperationIDs(ctx)
		if listErr = errors.Join(listErr, resource.Close()); listErr != nil {
			return nil, listErr
		}
		ids = append(ids, recorded...)
	}
	return ids, nil
}
