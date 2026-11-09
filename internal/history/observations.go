package history

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RefreshObservations spends a finite source-read budget on the oldest
// attempts. Failed reads preserve earlier evidence and rotate fairly. Published
// rows leave this frequent queue; selected rows always reread source facts.
func (c *Catalog) RefreshObservations(ctx context.Context, limit int) (progress Progress, resultErr error) {
	c.use.RLock()
	defer c.use.RUnlock()
	c.scan.Lock()
	defer c.scan.Unlock()
	if limit == 0 {
		limit = DefaultObservationBatch
	}
	if limit < 1 || limit > ScanBatchSize {
		return progress, errors.New("observation batch limit must be 1..256")
	}
	if err := c.check(ctx); err != nil {
		return progress, err
	}
	if err := c.claimWriter(ctx); err != nil {
		return progress, err
	}
	if err := c.refreshObservationBatch(ctx, limit); err != nil {
		return progress, err
	}
	return c.progress(ctx)
}

// refreshObservationBatch runs only while the caller owns Catalog's use/scan
// guards and elected writer role. ScanAndObserve shares this body rather than
// reentering public RefreshObservations and deadlocking its scan mutex.
func (c *Catalog) refreshObservationBatch(ctx context.Context, limit int) error {
	ids, err := c.pendingObservations(ctx, limit)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return err
		}
		value, readErr := observeSource(ctx, c.reader, id)
		if readErr != nil {
			if err = ctx.Err(); err != nil {
				return err
			}
			_, err = c.db.ExecContext(ctx, "UPDATE entries SET observation_fault='passive observation unavailable or invalid',refreshed=? WHERE kind=? AND id=?", time.Now().UTC().UnixNano(), string(id.Kind), id.TaskID)
		} else {
			_, err = c.db.ExecContext(ctx, `UPDATE entries SET observed=?,admission=?,liveness=?,publication=?,status=?,failure=?,source=?,observation_fault='',refreshed=? WHERE kind=? AND id=? AND (observed='' OR observed<=?)`,
				value.At.UTC().Format(timestampLayout), uint8(value.Admission), uint8(value.Liveness), uint8(value.Publication), value.Status, value.FailureCode, value.Source, time.Now().UTC().UnixNano(), string(id.Kind), id.TaskID, value.At.UTC().Format(timestampLayout))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) pendingObservations(ctx context.Context, limit int) (ids []Identity, resultErr error) {
	rows, err := c.db.QueryContext(ctx, "SELECT kind,id FROM entries WHERE fault='' AND (publication NOT IN (?,?) OR observed='') ORDER BY refreshed,kind,id LIMIT ?", uint8(task.PublicationCommitted), uint8(task.PublicationRejected), limit)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var id Identity
		if err = rows.Scan(&id.Kind, &id.TaskID); err != nil {
			return nil, err
		}
		if err = id.Validate(); err != nil {
			return nil, ErrCacheInvalid
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
