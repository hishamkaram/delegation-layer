package journal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

const turnBaselineRecordPrefix = "turn-baseline-"

// RecordTurnBaseline publishes one create-once prelaunch observation. It is
// accepted only while the exact reservation is open and its capture
// obligation names the same task subject; it never authorizes a launch.
func (r *Resource) RecordTurnBaseline(ctx context.Context, baseline contract.TurnBaseline) error {
	if err := contract.ValidateTurnBaseline(baseline, r.request.WorkspaceID, baseline.Sequence); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := requireOpenTaskUseTx(tx, r.request.WorkspaceID, baseline.Sequence, baseline.Subject); err != nil {
			return err
		}
		if err := baselineMatchesSourceTx(tx, baseline); err != nil {
			return err
		}
		name := turnBaselineName(baseline.Sequence)
		if _, err := tx.Put(name, baseline); err != nil {
			return err
		}
		return tx.Verify(name, baseline)
	})
}

// TurnBaseline reads the exact baseline for one reservation sequence. Only an
// authoritative absent read reports absence.
func (r *Resource) TurnBaseline(ctx context.Context, sequence uint64) (baseline contract.TurnBaseline, resultErr error) {
	if sequence == 0 {
		return baseline, task.ErrIdentityMismatch
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		baseline, err = readTurnBaselineTx(tx, r.request.WorkspaceID, sequence)
		return err
	})
	return baseline, resultErr
}

// TurnBaselines enumerates every committed baseline for forget inventory.
// A family-shaped name that does not decode is an evidence fault.
func (r *Resource) TurnBaselines(ctx context.Context) (baselines []contract.TurnBaseline, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		names, err := r.control.RecordNamesContext(ctx)
		if err != nil {
			return err
		}
		for _, name := range names {
			sequence, family, parseErr := parseTurnBaselineName(name)
			if parseErr != nil {
				return parseErr
			}
			if !family {
				continue
			}
			baseline, readErr := readTurnBaselineTx(tx, r.request.WorkspaceID, sequence)
			if readErr != nil {
				return requireResourceRecord(readErr)
			}
			baselines = append(baselines, baseline)
		}
		return nil
	})
	if resultErr != nil {
		return nil, resultErr
	}
	slices.SortFunc(baselines, func(first, second contract.TurnBaseline) int {
		return compareSequence(first.Sequence, second.Sequence)
	})
	return baselines, nil
}

// ReservationAt reads one exact reservation, whether open or closed. It lets a
// task projection select its own use instead of the resource's latest use.
func (r *Resource) ReservationAt(ctx context.Context, sequence uint64) (reservation contract.Reservation, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		reservation, err = readReservationTx(tx, r.request.WorkspaceID, sequence)
		return err
	})
	return reservation, resultErr
}

func requireOpenTaskUseTx(tx *taskdir.ControlTransaction, workspaceID string, sequence uint64, subject contract.Subject) error {
	if _, err := readReservationTx(tx, workspaceID, sequence); err != nil {
		return requireResourceRecord(err)
	}
	if _, err := readReservationClosedTx(tx, workspaceID, sequence); err == nil {
		return fmt.Errorf("%w: workspace reservation is closed", task.ErrInvalidPermit)
	} else if !task.RecordAbsent(err) {
		return err
	}
	var obligation contract.CaptureObligation
	name := obligationName(sequence)
	if err := tx.Read(name, &obligation); err != nil {
		return requireResourceRecord(err)
	}
	if err := validateCaptureObligationRecord(obligation, workspaceID, sequence); err != nil {
		return classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(name, obligation); err != nil {
		return err
	}
	if obligation.Subject != subject {
		return task.ErrIdentityMismatch
	}
	return nil
}

func baselineMatchesSourceTx(tx *taskdir.ControlTransaction, baseline contract.TurnBaseline) error {
	source, err := readSourceTx(tx)
	if err != nil {
		if task.RecordAbsent(err) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	if baseline.BaseOID != source.BaseOID || baseline.ObjectFormat != source.ObjectFormat {
		return task.ErrIdentityMismatch
	}
	return nil
}

func readTurnBaselineTx(tx *taskdir.ControlTransaction, workspaceID string, sequence uint64) (contract.TurnBaseline, error) {
	var baseline contract.TurnBaseline
	name := turnBaselineName(sequence)
	if err := tx.Read(name, &baseline); err != nil {
		return contract.TurnBaseline{}, classifyResourceRecordReadError(err)
	}
	if err := contract.ValidateTurnBaseline(baseline, workspaceID, sequence); err != nil {
		return contract.TurnBaseline{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(name, baseline); err != nil {
		return contract.TurnBaseline{}, err
	}
	if err := baselineMatchesSourceTx(tx, baseline); err != nil {
		return contract.TurnBaseline{}, classifyStoredResourceRecordError(err)
	}
	return baseline, nil
}

func turnBaselineName(sequence uint64) string {
	return fmt.Sprintf("%s%0*d.json", turnBaselineRecordPrefix, reservationSequenceWidth, sequence)
}

func parseTurnBaselineName(name string) (uint64, bool, error) {
	if !strings.HasPrefix(name, turnBaselineRecordPrefix) {
		return 0, false, nil
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, turnBaselineRecordPrefix), ".json")
	sequence, err := strconv.ParseUint(stem, 10, 64)
	if err != nil || sequence == 0 || turnBaselineName(sequence) != name {
		return 0, true, task.ErrEvidenceFault
	}
	return sequence, true, nil
}

func compareSequence(first, second uint64) int {
	switch {
	case first < second:
		return -1
	case first > second:
		return 1
	default:
		return 0
	}
}
