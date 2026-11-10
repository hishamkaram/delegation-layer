package history

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Catalog has no background goroutines. The session owns scheduling, cancelling
// and joining Scan/RefreshObservations. Close joins callers via the use lease.
type Catalog struct {
	use  sync.RWMutex
	role sync.Mutex
	scan sync.Mutex
	db   *sql.DB
	// The per-instance SQL construction port is immutable while a catalog is in
	// use. Nil selects SQLite; ownership tests can exercise actual driver faults.
	connector driver.Connector
	// database/sql forgets a driver handle even when its Close fails. This
	// failure is terminal custody, not a retryable native-close operation.
	dbCloseErr error
	reader     *taskdir.ReadOnlyStore
	viewer     *taskdir.ReadOnlyStore
	useLease   *taskdir.LockFile
	writer     *taskdir.LockFile
	pin        taskdir.RootPin
	registry   predicate.Registry
	path       string
	closed     bool
	leader     bool
	rebuilt    bool
	// afterScanBatch is a zero-value deterministic ownership-test gate. It runs
	// after a committed metadata/optional observation batch, never inside a SQL transaction.
	afterScanBatch func(context.Context) error
}

// Open returns a retained Catalog together with an error only when failed
// startup cleanup still owns native handles/leases. The caller must retain that
// handle; an error never grants permission to replace possibly open cache files.
func Open(ctx context.Context, options Options) (value *Catalog, resultErr error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	registry := options.Registry
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, options.Root, registry)
	if err != nil {
		return nil, err
	}
	c := &Catalog{reader: reader, registry: registry}
	defer func() {
		if resultErr != nil {
			value, resultErr = c.failedOpen(resultErr)
		}
	}()
	c.pin, err = reader.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	c.viewer, err = taskdir.OpenReadOnlyStoreContext(ctx, reader.Root(), registry)
	if err != nil {
		return nil, err
	}
	c.path, err = cachePath(options.Environment, c.pin.Root())
	if err != nil {
		return nil, err
	}
	if err = c.openCache(ctx, options.Rebuild); err != nil {
		return nil, err
	}
	return c, nil
}

// failedOpen preserves cleanup custody when construction cannot finish. An
// error alone must never make the native use/scanner lease owner unreachable.
func (c *Catalog) failedOpen(cause error) (*Catalog, error) {
	closeErr := c.Close()
	if closeErr != nil {
		return c, errors.Join(cause, closeErr)
	}
	return nil, cause
}

func (c *Catalog) Root() string { return c.pin.Root() }

func (c *Catalog) RootID() string { return c.pin.RootID() }

func (c *Catalog) CachePath() string { return c.path }

// Rebuilt reports discarded invalid data; a first empty catalog is not repair.
func (c *Catalog) Rebuilt() bool { return c.rebuilt }

func (c *Catalog) Close() error {
	c.use.Lock()
	defer c.use.Unlock()
	c.closed = true
	if err := c.closeDatabase(); err != nil {
		return err
	}
	var err error
	if c.writer != nil {
		if err = c.writer.Close(); err != nil {
			return err
		}
		c.writer = nil
	}
	if c.useLease != nil {
		if err = c.useLease.Close(); err != nil {
			return err
		}
		c.useLease = nil
	}
	if c.viewer != nil {
		err = errors.Join(err, c.viewer.Close())
	}
	if c.reader != nil {
		err = errors.Join(err, c.reader.Close())
	}
	return err
}

// closeDatabase runs under exclusive use ownership or unpublished startup.
// SQL's idempotent second Close is not proof that a failed driver Close settled.
// Keep the first error and all catalog leases until process exit.
func (c *Catalog) closeDatabase() error {
	if c.dbCloseErr != nil {
		return c.dbCloseErr
	}
	if c.db == nil {
		return nil
	}
	if err := c.db.Close(); err != nil {
		c.dbCloseErr = errors.Join(ErrCloseUnresolved, err)
		return c.dbCloseErr
	}
	c.db = nil
	return nil
}

func (c *Catalog) check(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if c.closed {
		return os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Reopen metadata as well as native identity: an old RootPin is not a
	// permanent authority when root.json or the root directory was replaced.
	fresh, err := taskdir.OpenReadOnlyStoreContext(ctx, c.pin.Root(), c.registry)
	if err != nil {
		return errors.Join(ErrRootChanged, err)
	}
	pin, pinErr := fresh.PinRootContext(ctx)
	closeErr := fresh.Close()
	if pinErr != nil || !pin.Equal(c.pin) {
		return errors.Join(ErrRootChanged, pinErr, closeErr)
	}
	return closeErr
}

func (c *Catalog) claimWriter(ctx context.Context) error {
	c.role.Lock()
	defer c.role.Unlock()
	if c.leader {
		return nil
	}
	if err := c.writer.LockEXNonblockingContext(ctx); err != nil {
		if lockBusy(err) {
			return ErrNotWriter
		}
		return err
	}
	c.leader = true
	return nil
}

func (c *Catalog) isWriter() bool {
	c.role.Lock()
	defer c.role.Unlock()
	return c.leader
}

type binding struct {
	Root, RootID  string
	Device, Inode uint64
}

func (c *Catalog) binding() (string, error) {
	data, err := json.Marshal(binding{c.pin.Root(), c.pin.RootID(), c.pin.Device(), c.pin.Inode()})
	return string(data), err
}
