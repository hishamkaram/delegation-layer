package testsupport

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const collectionFixtureLine = "delegation collection fixture brief line\n"

// HeldCollectionCohort owns one store whose tasks each have a consumed
// StartPermit and a held real RunnerLease. Nothing is sealed, so every
// collection stays a pending publication and no provider, supervisor, or
// catalog is involved. The cohort is the only owner of those leases; callers
// observe the tasks through handles from OpenCollector.
type HeldCollectionCohort struct {
	root    string
	taskIDs []string
	store   *taskdir.Store
	holders []heldCollectionTask
	closed  bool
	closeEr error
}

type heldCollectionTask struct {
	td     *taskdir.TaskDir
	permit *taskdir.StartPermit
}

// CollectionHandle is one independent Store and TaskDir pair over a cohort
// task. Concurrent collectors each need their own handle.
type CollectionHandle struct {
	store *taskdir.Store
	td    *taskdir.TaskDir
	done  bool
	err   error
}

// FixtureBrief returns the deterministic brief of exactly size bytes.
func FixtureBrief(size int) []byte {
	return []byte(strings.Repeat(collectionFixtureLine, size/len(collectionFixtureLine)+1))[:size]
}

// NewHeldCollectionCohort creates tasks fixture tasks that share one
// workspace, each with a brief of briefSize bytes and a held runner lease.
// Task identities are deterministic. Cleanup is registered on tb and may also
// be run earlier through Close.
func NewHeldCollectionCohort(tb testing.TB, briefSize, tasks int) *HeldCollectionCohort {
	tb.Helper()
	if briefSize < 1 || briefSize > task.MaxBriefSize || tasks < 1 || tasks > 16 {
		tb.Fatalf("invalid cohort shape: brief %d bytes, %d tasks (1..16)", briefSize, tasks)
	}
	base, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	store, err := taskdir.InitStore(filepath.Join(base, "state"))
	if err != nil {
		tb.Fatal(err)
	}
	cohort := &HeldCollectionCohort{root: store.Root, store: store}
	tb.Cleanup(func() {
		if closeErr := cohort.Close(); closeErr != nil {
			tb.Error(closeErr)
		}
	})
	brief := FixtureBrief(briefSize)
	paths := appFixturePaths{
		provider: filepath.Join(base, "inert-provider"),
		client:   filepath.Join(base, "inert-pueue"),
		endpoint: "unix:" + filepath.Join(base, "inert-pueue.sock"),
		config:   filepath.Join(base, "inert-pueue.yml"),
	}
	for index := range tasks {
		id := strings.Repeat(string("0123456789abcdef"[index]), 32)
		req, meta := appFixtureRecords(store.RootID, id, workspace, brief, nil, paths)
		td, createErr := store.CreateTask(id, req, brief, meta)
		if createErr != nil {
			tb.Fatal(createErr)
		}
		cohort.taskIDs = append(cohort.taskIDs, id)
		// The holder is registered before its lease so a failed start still
		// closes its TaskDir; Release of a nil permit is a no-op.
		cohort.holders = append(cohort.holders, heldCollectionTask{td: td})
		permit, holdErr := holdRunnerLease(td)
		if holdErr != nil {
			tb.Fatal(holdErr)
		}
		cohort.holders[index].permit = permit
	}
	return cohort
}

// holdRunnerLease takes the real runner lease, consumes its permit, and
// records the started state without sealing, so the lease stays held.
func holdRunnerLease(td *taskdir.TaskDir) (*taskdir.StartPermit, error) {
	permit, err := td.PrepareStart(0)
	if err != nil {
		return nil, err
	}
	if err = permit.Consume(); err != nil {
		return nil, errors.Join(err, permit.Release())
	}
	if err = td.RecordStarted(0); err != nil {
		return nil, errors.Join(err, permit.Release())
	}
	return permit, nil
}

// Root is the store root a collection call takes.
func (c *HeldCollectionCohort) Root() string { return c.root }

// TaskIDs returns a copy of the cohort's task identities.
func (c *HeldCollectionCohort) TaskIDs() []string {
	return append([]string(nil), c.taskIDs...)
}

// OpenCollector opens an independent Store and TaskDir over task index. The
// handle is closed by tb cleanup if the caller has not closed it.
func (c *HeldCollectionCohort) OpenCollector(tb testing.TB, index int) *CollectionHandle {
	tb.Helper()
	if index < 0 || index >= len(c.taskIDs) {
		tb.Fatalf("collector index %d outside %d tasks", index, len(c.taskIDs))
	}
	store, err := taskdir.OpenStore(c.root)
	if err != nil {
		tb.Fatal(err)
	}
	td, err := store.OpenTask(c.taskIDs[index])
	if err != nil {
		tb.Fatal(errors.Join(err, store.Close()))
	}
	handle := &CollectionHandle{store: store, td: td}
	tb.Cleanup(func() {
		if closeErr := handle.Close(); closeErr != nil {
			tb.Error(closeErr)
		}
	})
	return handle
}

// TaskDir is the handle's task directory. It is valid until Close.
func (h *CollectionHandle) TaskDir() *taskdir.TaskDir { return h.td }

// Close closes the handle's TaskDir and then its Store. It is idempotent.
func (h *CollectionHandle) Close() error {
	if !h.done {
		h.done = true
		h.err = errors.Join(h.td.Close(), h.store.Close())
	}
	return h.err
}

// Close releases every held lease before closing the TaskDirs and the Store.
// Callers join any collector worker first and never close concurrently. It is
// idempotent.
func (c *HeldCollectionCohort) Close() error {
	if c.closed {
		return c.closeEr
	}
	c.closed = true
	var errs []error
	for _, held := range c.holders {
		errs = append(errs, held.permit.Release())
	}
	for _, held := range c.holders {
		errs = append(errs, held.td.Close())
	}
	errs = append(errs, c.store.Close())
	c.closeEr = errors.Join(errs...)
	return c.closeEr
}
