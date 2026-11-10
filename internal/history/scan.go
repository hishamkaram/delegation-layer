package history

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Scan performs one finite pass. Session scheduling controls cadence. The
// connection-local seen set spills to SQLite TEMP storage and is never a
// generation-stamp update of every persisted metadata row.
func (c *Catalog) Scan(ctx context.Context) (Progress, error) {
	return c.scanPass(ctx, false)
}

// ScanAndObserve interleaves one bounded passive observation batch after each
// committed metadata batch. The same elected catalog writer owns both updates;
// neither source task state nor a second observation authority is introduced.
func (c *Catalog) ScanAndObserve(ctx context.Context) (Progress, error) {
	return c.scanPass(ctx, true)
}

func (c *Catalog) scanPass(ctx context.Context, observe bool) (progress Progress, resultErr error) {
	c.use.RLock()
	defer c.use.RUnlock()
	c.scan.Lock()
	defer c.scan.Unlock()
	if err := c.check(ctx); err != nil {
		return progress, err
	}
	if err := c.claimWriter(ctx); err != nil {
		return progress, err
	}
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return progress, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeScanConnection(ctx, conn)) }()
	if _, err = conn.ExecContext(ctx, "DROP TABLE IF EXISTS temp.seen; CREATE TEMP TABLE seen(kind TEXT,id TEXT,PRIMARY KEY(kind,id)); UPDATE catalog SET incomplete=1,scan_faults=0 WHERE singleton=1"); err != nil {
		return progress, err
	}
	complete := true
	var invalid int64
	for _, kind := range []taskdir.IndexKind{taskdir.IndexLocal, taskdir.IndexRemote} {
		clean, faults, scanErr := c.scanFamily(ctx, conn, kind, observe)
		if scanErr != nil {
			return progress, scanErr
		}
		complete = complete && clean
		invalid += faults
	}
	if err = c.check(ctx); err != nil {
		return progress, err
	}
	if complete {
		if err = c.sweep(ctx, conn); err != nil {
			return progress, err
		}
		_, err = conn.ExecContext(ctx, "UPDATE catalog SET incomplete=0,last_scan=?,scan_faults=? WHERE singleton=1", time.Now().UTC().UnixNano(), invalid)
	} else {
		_, err = conn.ExecContext(ctx, "UPDATE catalog SET scan_faults=? WHERE singleton=1", invalid)
	}
	if err != nil {
		return progress, err
	}
	return c.progress(ctx)
}

// The scan owns TEMP storage through its reserved connection. Dropping seen
// before returning that connection prevents retention until the next scan;
// cancellation does not revoke this finite cleanup attempt.
func closeScanConnection(ctx context.Context, conn *sql.Conn) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_, dropErr := conn.ExecContext(cleanup, "DROP TABLE IF EXISTS temp.seen")
	return errors.Join(dropErr, conn.Close())
}

func (c *Catalog) scanFamily(ctx context.Context, conn *sql.Conn, kind taskdir.IndexKind, observe bool) (clean bool, invalid int64, resultErr error) {
	scan, err := c.reader.OpenIndexScan(ctx, kind)
	if err != nil {
		return false, 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, scan.Close()) }()
	clean = true
	for {
		candidates, done, err := scan.Next(ctx, ScanBatchSize)
		if err != nil {
			return false, invalid, err
		}
		batch, batchClean, faults := c.readMetadataBatch(ctx, kind, candidates)
		invalid += faults
		clean = clean && batchClean
		invariantFault, err := storeMetadataBatch(ctx, conn, batch)
		if err != nil {
			return false, invalid, err
		}
		clean = clean && !invariantFault
		if err = c.finishScanBatch(ctx, observe); err != nil {
			return false, invalid, err
		}

		if done {
			return clean, invalid, nil
		}
	}
}

// finishScanBatch is a safe scheduling boundary: no metadata transaction is
// open while passive evidence is read or the deterministic test gate waits.
func (c *Catalog) finishScanBatch(ctx context.Context, observe bool) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	if observe {
		if err := c.refreshObservationBatch(ctx, DefaultObservationBatch); err != nil {
			return err
		}
	}
	if c.afterScanBatch != nil {
		return c.afterScanBatch(ctx)
	}
	return nil
}

func (c *Catalog) readMetadataBatch(ctx context.Context, kind taskdir.IndexKind, candidates []taskdir.IndexCandidate) ([]Row, bool, int64) {
	batch := make([]Row, 0, len(candidates))
	clean := true
	var invalid int64
	for _, candidate := range candidates {
		if task.ValidateTaskID(candidate.ID) != nil {
			invalid++
			continue
		}
		id := Identity{Kind: kind, TaskID: candidate.ID}
		row := Row{Identity: id}
		err := candidate.Err
		if err == nil {
			row, err = c.readMetadata(ctx, id)
		}
		if err != nil {
			// A valid directory without its task request or origin intent is an
			// interrupted/pending commit, not incomplete enumeration. Its ID is
			// still seen and prior metadata/fault is retained. Unsafe entries
			// and other source errors keep the no-sweep guard.
			partial := candidate.Err == nil && task.RecordAbsent(err)
			clean = clean && partial
			row.Fault = "source metadata unavailable or invalid"
		}
		batch = append(batch, row)
	}
	return batch, clean, invalid
}

func storeMetadataBatch(ctx context.Context, conn *sql.Conn, batch []Row) (invariantFault bool, resultErr error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	for _, row := range batch {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO seen(kind,id) VALUES(?,?)", string(row.Kind), row.TaskID); err != nil {
			return false, err
		}
		fault, err := storeMetadata(ctx, tx, row)
		if err != nil {
			return false, err
		}
		invariantFault = invariantFault || fault
	}
	return invariantFault, tx.Commit()
}

const immutableMetadataFault = "immutable source metadata changed"

func storeMetadata(ctx context.Context, tx *sql.Tx, row Row) (bool, error) {
	var request, metadata, fault string
	err := tx.QueryRowContext(ctx, "SELECT request_digest,metadata_digest,fault FROM entries WHERE kind=? AND id=?", string(row.Kind), row.TaskID).Scan(&request, &metadata, &fault)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if row.Fault != "" {
		if fault == immutableMetadataFault {
			// A later partial read cannot clear observed immutable drift. Only
			// validating the original metadata again can settle that fault.
			return true, nil
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO entries(kind,id,fault) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET fault=excluded.fault WHERE fault<>excluded.fault", string(row.Kind), row.TaskID, row.Fault)
		return false, err
	}
	m := row.Metadata
	if (request != "" && request != m.RequestDigest) || (metadata != "" && metadata != m.MetadataDigest) {
		_, err = tx.ExecContext(ctx, "UPDATE entries SET fault=? WHERE kind=? AND id=? AND fault<>?", immutableMetadataFault, string(row.Kind), row.TaskID, immutableMetadataFault)
		return true, err
	}
	if err == nil && request == m.RequestDigest && metadata == m.MetadataDigest && fault == "" {
		return false, nil
	}
	search := strings.ToLower(strings.Join([]string{row.Handle(), m.Provider, m.Model, m.Effort, m.Cwd, m.WorkspaceID, m.TeamID, m.MemberID, row.Device}, "\n"))
	_, err = tx.ExecContext(ctx, `INSERT INTO entries(kind,id,provider,model,effort,cwd,workspace,team,member,device,created,request_digest,metadata_digest,search,fault)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'') ON CONFLICT(kind,id) DO UPDATE SET provider=excluded.provider,model=excluded.model,effort=excluded.effort,cwd=excluded.cwd,workspace=excluded.workspace,team=excluded.team,member=excluded.member,device=excluded.device,created=excluded.created,request_digest=excluded.request_digest,metadata_digest=excluded.metadata_digest,search=excluded.search,fault=''`,
		string(row.Kind), row.TaskID, m.Provider, m.Model, m.Effort, m.Cwd, m.WorkspaceID, m.TeamID, m.MemberID, row.Device, createdValue(m), m.RequestDigest, m.MetadataDigest, search)
	return false, err
}

func (c *Catalog) sweep(ctx context.Context, conn *sql.Conn) error {
	var afterKind, afterID string
	for {
		ids, err := unseenEntries(ctx, conn, afterKind, afterID)
		if err != nil || len(ids) == 0 {
			return err
		}
		for _, id := range ids {
			present, err := c.reader.IndexEntryExists(ctx, id.Kind, id.TaskID)
			if err != nil {
				return err
			}
			if !present {
				if _, err = conn.ExecContext(ctx, "DELETE FROM entries WHERE kind=? AND id=?", string(id.Kind), id.TaskID); err != nil {
					return err
				}
			}
			afterKind, afterID = string(id.Kind), id.TaskID
		}
	}
}

func unseenEntries(ctx context.Context, conn *sql.Conn, kind, id string) (values []Identity, resultErr error) {
	rows, err := conn.QueryContext(ctx, `SELECT kind,id FROM entries e WHERE (kind>? OR (kind=? AND id>?)) AND NOT EXISTS(SELECT 1 FROM seen s WHERE s.kind=e.kind AND s.id=e.id) ORDER BY kind,id LIMIT ?`, kind, kind, id, ScanBatchSize)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var value Identity
		if err = rows.Scan(&value.Kind, &value.TaskID); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// Fixed-width UTC text orders the complete RFC3339 year range; UnixNano cannot
// represent valid timestamps before 1678 or after 2262.
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func createdValue(value taskdir.IndexMetadata) string {
	if !value.CreatedAtKnown {
		return ""
	}
	return value.CreatedAt.UTC().Format(timestampLayout)
}
