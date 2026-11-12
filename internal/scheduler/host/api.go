// Package host owns the user-started scheduler host, its exclusive H lease,
// intent journal, authenticated controls and single child Start/Wait lifecycle.
// The child application owns E and SQLite calendar/task authority.
package host

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	ProtocolVersion  = 1
	ChildReadFDFlag  = "--scheduler-host-read-fd"
	ChildWriteFDFlag = "--scheduler-host-write-fd"
	// ChildBusyExitCode is reserved for a resident that could not acquire E.
	// The host reports waiting_for_prior_runtime instead of a runtime crash.
	ChildBusyExitCode = 3
)

var (
	ErrCleanupUnresolved = errors.New("scheduler child cleanup unresolved")
	ErrPending           = errors.New("scheduler host operation pending")
	ErrIncompatible      = errors.New("scheduler host protocol or definition incompatible")
	ErrIdentity          = errors.New("scheduler host identity changed")
)

// Config is an explicit, retained launch context. RunnerExecutable is the
// installation resolver's selected managed runner. Environment must be nonnil;
// an empty slice intentionally supplies an empty environment. No launch context
// is written to durable host records or public diagnostics.
type Config struct {
	Store            *taskdir.Store
	Executable       string
	RunnerExecutable string
	Args             []string
	Environment      []string
	Directory        string
}

// Query consumes an already opened passive reader. Neither Observe nor Stop
// initializes a root, creates directories, migrates or starts infrastructure.
type Query struct {
	Store *taskdir.ReadOnlyStore
}

type Identity struct {
	PID        int    `json:"pid"`
	StartToken string `json:"start_token"`
}

// Status is a projection of the host loop and exact owner observations. Ready
// requires both HostReady and available installation health in observations.
// No response grants launch authority to a client.
type Status struct {
	State        string              `json:"state"`
	HostReady    bool                `json:"host_ready"`
	Health       *InstallationHealth `json:"health,omitempty"`
	Epoch        string              `json:"epoch,omitempty"`
	Attempt      uint64              `json:"attempt,omitempty"`
	Ready        bool                `json:"ready"`
	Fault        bool                `json:"fault,omitempty"`
	NextRetryAt  *time.Time          `json:"next_retry_at,omitempty"`
	LastError    string              `json:"last_error,omitempty"`
	StopAccepted bool                `json:"stop_accepted"`
	Protocol     int                 `json:"protocol"`
	BuildID      string              `json:"build_id,omitempty"`
	Host         Identity            `json:"host"`
	Child        *Identity           `json:"child,omitempty"`
}

type StopResult struct {
	Status   Status
	Accepted bool
	Stopped  bool
}

// ChildConfig binds inherited pollable pipes to this application's already
// held E lease. RunChild completes live confirmation before invoking Run.
type ChildConfig struct {
	RootID      string
	ReadFD      int
	WriteFD     int
	BeforeReady func(context.Context) error
}

func CheckCompatible(status Status) error {
	if status.Protocol != ProtocolVersion {
		return ErrIncompatible
	}
	return nil
}
