// Package history owns a disposable, metadata-only task search catalog. The
// task and remote stores remain authoritative; catalog rows grant no authority.
package history

import (
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var (
	ErrCacheBusy       = errors.New("history cache is in use; rebuild deferred")
	ErrNotWriter       = errors.New("another session is indexing history")
	ErrRootChanged     = errors.New("history source binding changed; reopen the context")
	ErrCacheInvalid    = errors.New("history cache is invalid; rebuild required")
	ErrCloseUnresolved = errors.New("history database close failed; cache custody is retained until this process exits; restart before reopening or rebuilding the cache")
)

const (
	ScanBatchSize           = 256
	DefaultPageSize         = 50
	DefaultObservationBatch = 32
	DefaultScanInterval     = 10 * time.Second
	DefaultRefreshInterval  = time.Second
	DefaultVisibleInterval  = 2 * time.Second
	maximumPageSize         = 200
)

type Options struct {
	Root        string
	Environment []string
	Registry    predicate.Registry
	// Rebuild discards only this root's owned cache under exclusive use lease.
	// Close existing session handles before requesting it.
	Rebuild bool
}

type Identity struct {
	Kind   taskdir.IndexKind
	TaskID string
}

func (id Identity) Validate() error {
	if id.Kind != taskdir.IndexLocal && id.Kind != taskdir.IndexRemote {
		return fmt.Errorf("invalid history source kind %q", id.Kind)
	}
	return task.ValidateTaskID(id.TaskID)
}

func (id Identity) Handle() string {
	if id.Kind == taskdir.IndexRemote {
		return "remote:" + id.TaskID
	}
	return id.TaskID
}

type Observation struct {
	At          time.Time
	Admission   task.Admission
	Liveness    task.Liveness
	Publication task.Publication
	Status      string
	FailureCode string
	Source      string
}

// ObservationResult preserves an entry-specific fault without erasing a
// previous observation. Binding/cancellation failures belong to the whole batch.
type ObservationResult struct {
	Observation Observation
	Err         error
}

type Row struct {
	Identity
	Metadata         taskdir.IndexMetadata
	Device           string
	Observation      *Observation
	Fault            string
	ObservationFault string
}

type Order string

const (
	OrderCreated  Order = "created"
	OrderObserved Order = "observed"
)

// Filters apply to dated observations, never a claim of global current state.
type Query struct {
	Search, Admission, Liveness, Publication, Status string
	After                                            string
	Limit                                            int
	Order                                            Order
}

type Progress struct {
	Indexed, Observed, Faults, Unobserved int64
	LastScan                              time.Time
	Incomplete                            bool
	Writer                                bool
}

type Page struct {
	Rows     []Row
	Next     string
	Progress Progress
}
