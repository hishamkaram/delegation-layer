package payload

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var (
	ErrRetired           = taskdir.ErrRemoteDataRetired
	ErrRetirementBlocked = errors.New("remote data retirement is blocked")
)

type (
	RetirementRecord struct {
		Version       int                   `json:"version"`
		RootID        string                `json:"root_id"`
		TaskID        string                `json:"task_id"`
		Role          remotedata.Role       `json:"role"`
		StorageLayout task.RemoteDataLayout `json:"storage_layout,omitempty"`
		Families      []remotedata.Family   `json:"families"`
		CreatedAt     string                `json:"created_at"`
	}
	RetirementReport struct {
		Role          remotedata.Role     `json:"role"`
		LayoutVersion int                 `json:"layout_version"`
		Retirable     bool                `json:"retirable"`
		Applied       bool                `json:"applied"`
		AlreadyDone   bool                `json:"already_done"`
		Blockers      []string            `json:"blockers"`
		Families      []remotedata.Family `json:"families"`
	}
)

func checkRetiredTransaction(tx *taskdir.ControlTransaction) error {
	names, e := tx.RecordNames()
	if e != nil {
		return e
	}
	for _, n := range names {
		if n == "retirement.json" {
			return ErrRetired
		}
	}
	return nil
}

func validateRetirement(record RetirementRecord, rootID, id string, role remotedata.Role) error {
	if record.Version != 1 || record.RootID != rootID || record.TaskID != id || record.Role != role {
		return task.ErrIdentityMismatch
	}
	if err := record.StorageLayout.Validate(); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err != nil {
		return task.ErrEvidenceFault
	}
	expected := families(role)
	if len(record.Families) != len(expected) {
		return task.ErrEvidenceFault
	}
	for i, f := range expected {
		if record.Families[i] != f {
			return task.ErrEvidenceFault
		}
	}
	return nil
}

func ReadRetirement(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, role remotedata.Role) (record RetirementRecord, present bool, err error) {
	if role != remotedata.Origin && role != remotedata.Destination {
		return record, false, task.ErrEvidenceFault
	}
	err = reader.ReadRemoteRecord(ctx, id, role == remotedata.Origin, false, "retirement.json", &record)
	if errors.Is(err, os.ErrNotExist) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	err = validateRetirement(record, reader.RootID(), id, role)
	if err != nil {
		return record, false, err
	}
	layout, err := CurrentLayout(ctx, reader, id, role)
	if err == nil && layout != record.StorageLayout {
		err = task.ErrIdentityMismatch
	}
	return record, err == nil, err
}

func OriginRetired(ctx context.Context, store *taskdir.Store, id string) (retired bool, resultErr error) {
	reader, err := store.RemoteReader(ctx, store.Root)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	_, retired, err = ReadRetirement(ctx, reader, id, remotedata.Origin)
	return retired, err
}

// PreviewRetirement grants no mutation authority and creates no filesystem data.
func PreviewRetirement(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, role remotedata.Role) (report RetirementReport, resultErr error) {
	report.Role = role
	report.Families = families(role)
	if role == remotedata.Origin {
		without, e := originWithoutSnapshot(ctx, reader, id)
		if e != nil {
			return report, e
		}
		if without {
			report.Families = []remotedata.Family{}
			report.Retirable = true
			report.Blockers = []string{}
			return report, nil
		}
	}
	layout, err := CurrentLayout(ctx, reader, id, role)
	if err != nil {
		return report, err
	}
	report.LayoutVersion = layoutVersion(layout)
	report.Blockers, err = retirementBlockers(ctx, reader, id, role, layout, false)
	report.Retirable = err == nil && len(report.Blockers) == 0
	return report, err
}
