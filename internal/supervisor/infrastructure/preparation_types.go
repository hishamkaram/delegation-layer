package infrastructure

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type DispositionKind string

const (
	DispositionStarted   DispositionKind = "started"
	DispositionWarmReuse DispositionKind = "warm-reuse"
)

// Disposition reports the resident's observed preparation outcome. The request
// handler owns its construction: Started requires a successful real Start
// callback and its resolved context; WarmReuse requires canonical readiness
// without a Start. A partial failure after Start retains Started alongside the
// error. This value alone proves neither readiness nor native ownership.
type Disposition struct {
	Kind    DispositionKind        `json:"kind"`
	Context LaunchContextReference `json:"context"`
}

func (d Disposition) Validate() error {
	switch d.Kind {
	case DispositionStarted:
		return d.Context.Validate()
	case DispositionWarmReuse:
		if d.Context != (LaunchContextReference{}) {
			return ErrIdentity
		}
		return nil
	default:
		return ErrIdentity
	}
}

// MigrationReference selects the existing canonical migration body and its
// first matching projection in completion history, or the next offset when
// absent. The canonical journal owns migration and native closure proof. The
// receiver re-reads the selected slot and full resolved target; this selector
// identifies a state result, not a unique attempt or new mutable generation.
// Paths, bindings and executable selections do not travel in this selector.
type MigrationReference struct {
	RootID       string `json:"root_id"`
	RecordSHA256 string `json:"record_sha256"`
	HistoryIndex uint64 `json:"history_index"`
}

func (r MigrationReference) Validate() error {
	if err := errors.Join(task.ValidateRootID(r.RootID), task.ValidateSHA256(r.RecordSHA256)); err != nil {
		return errors.Join(ErrIdentity, err)
	}
	return nil
}

// MigrationResult is independent of Start disposition. A completed migration
// can report a successful Start and can retain an additional cleanup fault.
type MigrationResult string

const (
	MigrationCompleted MigrationResult = "completed"
	MigrationWarmReuse MigrationResult = "warm-reuse"
)

func (r MigrationResult) Validate() error {
	switch r {
	case MigrationCompleted, MigrationWarmReuse:
		return nil
	default:
		return ErrIdentity
	}
}
