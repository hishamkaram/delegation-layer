package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
)

// txStep is one step of an operation transaction. It must close every Rows it
// opens before returning.
type txStep func(ctx context.Context, tx *sql.Tx) error

// transact runs exactly one operation: it reserves one connection, begins one
// immediate transaction, runs guard then body, and owns the finalization.
// Only this call's own successful Commit or explicit Rollback, with the
// operation context still live, proves the physical connection is clean;
// any other outcome retires that connection so a possibly open transaction
// or held lock never returns to the pool.
func (s *Store) transact(ctx context.Context, guard, body txStep) (resultErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return classify(err)
	}
	var tx *sql.Tx
	ended, clean := false, false
	defer func() {
		if tx != nil && !ended {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
		resultErr = errors.Join(resultErr, releaseConnection(conn, clean && ctx.Err() == nil))
	}()
	tx, err = conn.BeginTx(ctx, nil)
	if err != nil {
		return classify(err)
	}
	stepErr := guard(ctx, tx)
	if stepErr == nil {
		stepErr = body(ctx, tx)
	}
	if stepErr != nil {
		ended = true
		rollbackErr := tx.Rollback()
		clean = rollbackErr == nil
		if errors.Is(rollbackErr, sql.ErrTxDone) && ctx.Err() != nil {
			// database/sql already rolled back for the done context; the
			// connection is still retired because clean is false.
			rollbackErr = nil
		}
		return errors.Join(stepErr, classify(rollbackErr))
	}
	ended = true
	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("%w: %w", errCommitOutcomeUnknown, classify(commitErr))
	}
	clean = true
	return nil
}

// releaseConnection returns a clean connection to the pool or retires it.
func releaseConnection(conn *sql.Conn, clean bool) error {
	if clean {
		return conn.Close()
	}
	// database/sql exposes the driver connection to Raw only as any. The
	// callback never reads it; returning ErrBadConn makes database/sql close
	// that physical connection instead of pooling it.
	rawErr := conn.Raw(func(any) error { return driver.ErrBadConn })
	closeErr := conn.Close()
	var faults []error
	if !errors.Is(rawErr, driver.ErrBadConn) {
		faults = append(faults, fmt.Errorf("retiring scheduler connection: %w", rawErr))
	}
	if closeErr != nil && !errors.Is(closeErr, sql.ErrConnDone) {
		faults = append(faults, fmt.Errorf("closing retired scheduler connection: %w", closeErr))
	}
	return errors.Join(faults...)
}
