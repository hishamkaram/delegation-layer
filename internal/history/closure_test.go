package history

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
)

// The synthetic connection owns no OS resource. sql.OpenDB keeps it private to
// this test; production SQLite registration and process-global state are intact.
type failingCloseConnector struct {
	failure      error
	pingFailure  error
	queryFailure error
	calls        *atomic.Int32
}

func (f failingCloseConnector) Connect(context.Context) (driver.Conn, error) {
	return failingCloseConnection(f), nil
}

func (failingCloseConnector) Driver() driver.Driver { return connectorOnlyDriver{} }

type connectorOnlyDriver struct{}

func (connectorOnlyDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("close fixture requires its instance connector")
}

type failingCloseConnection failingCloseConnector

func (f failingCloseConnection) Ping(context.Context) error { return f.pingFailure }

func (f failingCloseConnection) Prepare(string) (driver.Stmt, error) {
	if f.queryFailure != nil {
		return nil, f.queryFailure
	}
	return nil, errors.New("close fixture does not execute queries")
}

func (failingCloseConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("close fixture does not execute transactions")
}

func (f failingCloseConnection) Close() error {
	f.calls.Add(1)
	return f.failure
}

func TestCatalogSQLCloseFailureKeepsNativeCustodyUntilProcessExit(t *testing.T) {
	f := newSource(t)
	c, err := Open(t.Context(), f.options)
	require(t, err)
	var simulated *sql.DB
	t.Cleanup(func() {
		if simulated != nil && c.db == simulated {
			// Only this resource-free synthetic driver can be discharged in test
			// teardown. A real failed SQLite close is never unlatched at runtime.
			c.use.Lock()
			c.db, c.dbCloseErr = nil, nil
			c.use.Unlock()
		}
		require(t, c.Close())
	})
	_, err = c.Scan(t.Context())
	require(t, err)
	require(t, c.db.Close()) // Close real SQLite before installing the no-resource driver.
	failure := errors.New("controlled native driver close failure")
	var calls atomic.Int32
	simulated = sql.OpenDB(failingCloseConnector{failure: failure, calls: &calls})
	require(t, simulated.PingContext(t.Context()))
	c.db = simulated
	startupFailure := errors.New("controlled catalog construction failure")
	retained, firstErr := c.failedOpen(startupFailure)
	if retained != c || !errors.Is(firstErr, startupFailure) || !errors.Is(firstErr, failure) || !errors.Is(firstErr, ErrCloseUnresolved) {
		t.Fatalf("startup cleanup lost its native custody owner: %p %v", retained, firstErr)
	}
	// Go's DB is already marked closed, so its second Close cannot retry the
	// failed driver. The catalog must not mistake that nil for native closure.
	require(t, simulated.Close())
	latched := c.dbCloseErr
	if err = c.Close(); !errors.Is(err, latched) || !errors.Is(err, failure) || calls.Load() != 1 || c.db != simulated {
		t.Fatalf("SQL's idempotent Close cleared unresolved native custody: %v calls=%d", err, calls.Load())
	}
	if err = c.rebuild(t.Context()); !errors.Is(err, latched) {
		t.Fatalf("rebuild bypassed the retained SQL-close failure: %v", err)
	}
	assertCatalogLeasesRetained(t, c, f.options)
}

func TestRebuildFailureKeepsExclusiveLeaseThroughDatabaseClose(t *testing.T) {
	for _, phase := range []string{"connect", "initialize"} {
		t.Run(phase, func(t *testing.T) { checkRebuildFailureLease(t, phase) })
	}
}

func checkRebuildFailureLease(t *testing.T, phase string) {
	t.Helper()
	f := newSource(t)
	c, err := Open(t.Context(), f.options)
	require(t, err)
	original := c.db
	closeFailure := errors.New("controlled native driver close failure")
	t.Cleanup(func() {
		if c.db != nil && c.db != original {
			// Only the resource-free controlled driver may be unlatched here.
			// The real predecessor was closed before rebuild acquired EX.
			if closeErr := c.db.Close(); closeErr != nil && !errors.Is(closeErr, closeFailure) {
				t.Errorf("closing controlled driver: %v", closeErr)
			}
			c.use.Lock()
			c.db, c.dbCloseErr = nil, nil
			c.use.Unlock()
		}
		require(t, c.Close())
	})
	stageFailure := errors.New("controlled rebuild stage failure")
	var calls atomic.Int32
	connector := failingCloseConnector{failure: closeFailure, calls: &calls}
	if phase == "connect" {
		connector.pingFailure = stageFailure
	} else {
		connector.queryFailure = stageFailure
	}
	c.connector = connector
	err = c.rebuild(t.Context())
	if !errors.Is(err, stageFailure) || c.db == nil || calls.Load() != 0 {
		t.Fatalf("rebuild did not stop with its unclosed SQL handle: %v closes=%d", err, calls.Load())
	}
	assertExclusiveCacheLease(t, f.options)
	retained, cleanupErr := c.failedOpen(err)
	if retained != c || !errors.Is(cleanupErr, closeFailure) || !errors.Is(cleanupErr, ErrCloseUnresolved) || calls.Load() != 1 {
		t.Fatalf("failed rebuild cleanup lost database/lease custody: %p %v", retained, cleanupErr)
	}
	assertExclusiveCacheLease(t, f.options)
}

func assertExclusiveCacheLease(t *testing.T, options Options) {
	t.Helper()
	other, err := Open(t.Context(), options)
	if other != nil {
		require(t, other.Close())
	}
	if !errors.Is(err, ErrCacheBusy) {
		t.Fatalf("rebuild released EX while its database handle remained open: %v", err)
	}
}

func assertCatalogLeasesRetained(t *testing.T, c *Catalog, options Options) {
	t.Helper()
	if c.writer == nil || c.useLease == nil {
		t.Fatal("failed SQL close discarded native lease handles")
	}
	other, err := Open(t.Context(), options)
	require(t, err)
	t.Cleanup(func() { require(t, other.Close()) })
	if _, err = other.Scan(t.Context()); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("failed SQL close released sole scanner ownership: %v", err)
	}
	require(t, other.Close())
	options.Rebuild = true
	if replacement, openErr := Open(t.Context(), options); !errors.Is(openErr, ErrCacheBusy) {
		if replacement != nil {
			require(t, replacement.Close())
		}
		t.Fatalf("failed SQL close let another opener replace cache files: %v", openErr)
	}
}
