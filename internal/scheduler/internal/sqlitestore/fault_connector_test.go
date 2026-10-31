package sqlitestore

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

var errInjected = errors.New("injected finalization fault")

type finalizationFault uint8

const (
	faultNone finalizationFault = iota
	// faultCommitBeforeReal returns an error without committing, leaving the
	// real transaction open.
	faultCommitBeforeReal
	// faultCommitAfterReal commits for real, then returns an error.
	faultCommitAfterReal
	// faultRollback returns an error without rolling back.
	faultRollback
)

// faultConnector wraps the real modernc connector. Its connections expose
// exactly the behavior-relevant optional interfaces of the real connection,
// so database/sql takes the production pool and transaction paths.
type faultConnector struct {
	base  driver.Connector
	opens atomic.Int64
	mu    sync.Mutex
	fault finalizationFault
}

func (c *faultConnector) inject(fault finalizationFault) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fault = fault
}

// take returns and clears the armed fault so it applies to one transaction.
func (c *faultConnector) take() finalizationFault {
	c.mu.Lock()
	defer c.mu.Unlock()
	fault := c.fault
	c.fault = faultNone
	return fault
}

func (c *faultConnector) Driver() driver.Driver { return c.base.Driver() }

func (c *faultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	real, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped, err := wrapConn(real, c)
	if err != nil {
		return nil, errors.Join(err, real.Close())
	}
	c.opens.Add(1)
	return wrapped, nil
}

type faultConn struct {
	driver.Conn
	beginner  driver.ConnBeginTx
	preparer  driver.ConnPrepareContext
	execer    driver.ExecerContext
	queryer   driver.QueryerContext
	pinger    driver.Pinger
	resetter  driver.SessionResetter
	validator driver.Validator
	faults    *faultConnector
}

// wrapConn requires every optional interface the production path relies on,
// using checked assertions only.
func wrapConn(real driver.Conn, faults *faultConnector) (*faultConn, error) {
	beginner, beginOK := real.(driver.ConnBeginTx)
	preparer, prepareOK := real.(driver.ConnPrepareContext)
	execer, execOK := real.(driver.ExecerContext)
	queryer, queryOK := real.(driver.QueryerContext)
	pinger, pingOK := real.(driver.Pinger)
	resetter, resetOK := real.(driver.SessionResetter)
	validator, validOK := real.(driver.Validator)
	if !beginOK || !prepareOK || !execOK || !queryOK || !pingOK || !resetOK || !validOK {
		return nil, fmt.Errorf("driver connection %T lacks an expected optional interface", real)
	}
	return &faultConn{
		Conn: real, beginner: beginner, preparer: preparer, execer: execer, queryer: queryer,
		pinger: pinger, resetter: resetter, validator: validator, faults: faults,
	}, nil
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{real: tx, fault: c.faults.take()}, nil
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.preparer.PrepareContext(ctx, query)
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.execer.ExecContext(ctx, query, args)
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.queryer.QueryContext(ctx, query, args)
}

func (c *faultConn) Ping(ctx context.Context) error { return c.pinger.Ping(ctx) }

func (c *faultConn) ResetSession(ctx context.Context) error { return c.resetter.ResetSession(ctx) }

func (c *faultConn) IsValid() bool { return c.validator.IsValid() }

type faultTx struct {
	real  driver.Tx
	fault finalizationFault
}

func (t *faultTx) Commit() error {
	switch t.fault {
	case faultCommitBeforeReal:
		return errInjected
	case faultCommitAfterReal:
		return errors.Join(t.real.Commit(), errInjected)
	case faultNone, faultRollback:
		return t.real.Commit()
	default:
		return t.real.Commit()
	}
}

func (t *faultTx) Rollback() error {
	switch t.fault {
	case faultRollback:
		return errInjected
	case faultNone, faultCommitBeforeReal, faultCommitAfterReal:
		return t.real.Rollback()
	default:
		return t.real.Rollback()
	}
}
