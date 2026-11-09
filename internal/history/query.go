package history

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (c *Catalog) Query(ctx context.Context, query Query) (page Page, resultErr error) {
	c.use.RLock()
	defer c.use.RUnlock()
	if err := c.check(ctx); err != nil {
		return page, err
	}
	statement, args, err := buildQuery(query)
	if err != nil {
		return page, err
	}
	rows, err := c.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return page, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	limit := query.Limit
	if limit == 0 {
		limit = DefaultPageSize
	}
	for rows.Next() {
		row, readErr := readRow(rows)
		if readErr != nil {
			return page, readErr
		}
		if len(page.Rows) == limit {
			page.Next, err = encodeCursor(query, page.Rows[len(page.Rows)-1])
			if err != nil {
				return page, err
			}
			break
		}
		page.Rows = append(page.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	// Close the page cursor before reserving a connection for progress.
	if err = rows.Close(); err != nil {
		return page, err
	}
	page.Progress, err = c.progress(ctx)
	return page, err
}

type pageCursor struct {
	Key, Kind, ID, Filter string
	Order                 Order
}

func buildQuery(query Query) (string, []any, error) {
	if query.Limit == 0 {
		query.Limit = DefaultPageSize
	}
	if query.Limit < 1 || query.Limit > maximumPageSize || len(query.Search) > 512 || len(query.After) > 4096 {
		return "", nil, errors.New("history query exceeds bounds")
	}
	column, err := orderColumn(query.Order)
	if err != nil {
		return "", nil, err
	}
	where, args, err := queryFilters(query)
	if err != nil {
		return "", nil, err
	}
	if query.After != "" {
		cursor, err := decodeCursor(query)
		if err != nil {
			return "", nil, err
		}
		where = append(where, "("+column+"<? OR ("+column+"=? AND (kind>? OR (kind=? AND id>?))))")
		args = append(args, cursor.Key, cursor.Key, cursor.Kind, cursor.Kind, cursor.ID)
	}
	statement := "SELECT " + rowColumns + " FROM entries"
	if len(where) != 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += " ORDER BY " + column + " DESC,kind,id LIMIT ?"
	args = append(args, query.Limit+1)
	return statement, args, nil
}

func queryFilters(query Query) ([]string, []any, error) {
	var where []string
	var args []any
	if query.Search != "" {
		where, args = append(where, "instr(search,?)>0"), append(args, strings.ToLower(query.Search))
	}
	for _, filter := range []struct {
		column, value string
		values        []string
	}{
		{"admission", query.Admission, []string{"unknown", "not-admitted", "admitted"}},
		{"liveness", query.Liveness, []string{"undetermined", "running", "ended"}},
		{"publication", query.Publication, []string{"unknown", "pending", "committed", "rejected"}},
	} {
		if filter.value == "" {
			continue
		}
		index := stringIndex(filter.values, filter.value)
		if index < 0 {
			return nil, nil, fmt.Errorf("invalid last-observed %s filter", filter.column)
		}
		where = append(where, "observed<>''", filter.column+"=?")
		args = append(args, index)
	}
	if query.Status != "" {
		if len(query.Status) > 128 {
			return nil, nil, errors.New("status filter exceeds bounds")
		}
		where, args = append(where, "observed<>''", "status=?"), append(args, query.Status)
	}
	return where, args, nil
}

func stringIndex(values []string, value string) int {
	for i, v := range values {
		if v == value {
			return i
		}
	}
	return -1
}

func orderColumn(order Order) (string, error) {
	switch order {
	case "", OrderCreated:
		return "created", nil
	case OrderObserved:
		return "observed", nil
	default:
		return "", errors.New("history order must be created or observed")
	}
}

func filterKey(query Query) string {
	parts := []string{query.Search, query.Admission, query.Liveness, query.Publication, query.Status}
	for i, value := range parts {
		parts[i] = fmt.Sprintf("%d:%s", len(value), value)
	}
	return task.ComputeSHA256([]byte(strings.Join(parts, "")))
}

func encodeCursor(query Query, row Row) (string, error) {
	key := createdValue(row.Metadata)
	if query.Order == OrderObserved && row.Observation != nil {
		key = row.Observation.At.UTC().Format(timestampLayout)
	} else if query.Order == OrderObserved {
		key = ""
	}
	data, err := json.Marshal(pageCursor{Key: key, Kind: string(row.Kind), ID: row.TaskID, Filter: filterKey(query), Order: query.Order})
	return base64.RawURLEncoding.EncodeToString(data), err
}

func decodeCursor(query Query) (pageCursor, error) {
	var cursor pageCursor
	data, err := base64.RawURLEncoding.DecodeString(query.After)
	if err != nil {
		return cursor, errors.New("invalid history page cursor")
	}
	if err = task.DecodeStrict(data, &cursor); err != nil {
		return cursor, errors.New("invalid history page cursor")
	}
	if cursor.Order != query.Order || cursor.Filter != filterKey(query) {
		return cursor, errors.New("history cursor belongs to a different query")
	}
	if err = (Identity{Kind: taskdir.IndexKind(cursor.Kind), TaskID: cursor.ID}).Validate(); err != nil {
		return cursor, errors.New("invalid history page cursor")
	}
	if cursor.Key != "" {
		if _, err = time.Parse(timestampLayout, cursor.Key); err != nil {
			return cursor, errors.New("invalid history page cursor")
		}
	}
	return cursor, nil
}

const rowColumns = `kind,id,provider,model,effort,cwd,workspace,team,member,device,created,request_digest,metadata_digest,fault,observed,admission,liveness,publication,status,failure,source,observation_fault`

func readRow(rows *sql.Rows) (Row, error) {
	var row Row
	var created, observed string
	var status, failure, source string
	var admission, liveness, publication uint8
	err := rows.Scan(&row.Kind, &row.TaskID, &row.Metadata.Provider, &row.Metadata.Model, &row.Metadata.Effort, &row.Metadata.Cwd,
		&row.Metadata.WorkspaceID, &row.Metadata.TeamID, &row.Metadata.MemberID, &row.Device, &created, &row.Metadata.RequestDigest,
		&row.Metadata.MetadataDigest, &row.Fault, &observed, &admission, &liveness, &publication,
		&status, &failure, &source, &row.ObservationFault)
	if err != nil {
		return row, err
	}
	row.Metadata.TaskID = row.TaskID
	if err = row.Validate(); err != nil {
		return row, ErrCacheInvalid
	}
	if created != "" {
		row.Metadata.CreatedAt, err = time.Parse(timestampLayout, created)
		if err != nil {
			return row, ErrCacheInvalid
		}
		row.Metadata.CreatedAtKnown = true
	}
	if observed != "" {
		at, err := time.Parse(timestampLayout, observed)
		if err != nil || admission > uint8(task.AdmissionAdmitted) || liveness > uint8(task.LivenessEnded) || publication > uint8(task.PublicationRejected) {
			return row, ErrCacheInvalid
		}
		// Range checks above validate these exact enum narrowings.
		row.Observation = &Observation{At: at, Admission: task.Admission(admission), Liveness: task.Liveness(liveness), Publication: task.Publication(publication), Status: status, FailureCode: failure, Source: source}
	}
	return row, nil
}

func (c *Catalog) Progress(ctx context.Context) (Progress, error) {
	c.use.RLock()
	defer c.use.RUnlock()
	if err := c.check(ctx); err != nil {
		return Progress{}, err
	}
	return c.progress(ctx)
}

func (c *Catalog) progress(ctx context.Context) (Progress, error) {
	var value Progress
	var last int64
	var scanFaults int64
	if err := c.db.QueryRowContext(ctx, "SELECT incomplete,last_scan,scan_faults FROM catalog WHERE singleton=1").Scan(&value.Incomplete, &last, &scanFaults); err != nil {
		return value, err
	}
	if last != 0 {
		value.LastScan = time.Unix(0, last).UTC()
	}
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(observed<>''),0),COALESCE(SUM(fault<>'' OR observation_fault<>''),0) FROM entries").Scan(&value.Indexed, &value.Observed, &value.Faults); err != nil {
		return value, err
	}
	value.Faults += scanFaults
	value.Unobserved = value.Indexed - value.Observed
	value.Writer = c.isWriter()
	return value, nil
}
