package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const reservationSequenceWidth = 20

// Reservation returns the latest active reservation. A closed latest
// reservation means the resource is currently unused, not that an older
// reservation should be revived.
func (r *Resource) Reservation(ctx context.Context) (reservation Reservation, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		sequence, present, active, err := latestReservationHead(r.control, ctx)
		if err != nil {
			return err
		}
		if !present {
			return os.ErrNotExist
		}
		if !active {
			return errors.Join(task.ErrEvidenceFault, os.ErrNotExist)
		}
		reservation, err = readReservationTx(tx, r.request.WorkspaceID, sequence)
		if err != nil {
			return requireResourceRecord(err)
		}
		closed, closeErr := readReservationClosedTx(tx, r.request.WorkspaceID, sequence)
		if task.RecordAbsent(closeErr) {
			return nil
		}
		if closeErr != nil {
			return closeErr
		}
		if closed.WorkspaceID != reservation.WorkspaceID || closed.Sequence != reservation.Sequence {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		reservation = Reservation{}
		return os.ErrNotExist
	})
	return reservation, resultErr
}

// ReservationClosed reads one exact closure record. It never infers closure
// from a missing active reservation or from a native process observation.
func (r *Resource) ReservationClosed(ctx context.Context, sequence uint64) (closed ReservationClosed, resultErr error) {
	if sequence == 0 {
		return closed, task.ErrIdentityMismatch
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		closed, err = readReservationClosedTx(tx, r.request.WorkspaceID, sequence)
		return err
	})
	return closed, resultErr
}

// Reserve serializes every managed user irrespective of provider/session.
// The transaction ends before any external admission or command starts.
func (r *Resource) Reserve(ctx context.Context, subject Subject) (reservation Reservation, resultErr error) {
	if err := validateSubject(subject); err != nil {
		return reservation, err
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		return r.reserveTx(ctx, tx, subject, &reservation)
	})
	return reservation, resultErr
}

func (r *Resource) reserveTx(ctx context.Context, tx *taskdir.ControlTransaction, subject Subject, result *Reservation) error {
	if err := r.requireReservable(tx); err != nil {
		return err
	}
	sequence, present, active, err := latestReservationHead(r.control, ctx)
	if err != nil {
		return err
	}
	if !present {
		*result = Reservation{SchemaVersion: task.SchemaVersion, WorkspaceID: r.request.WorkspaceID, Sequence: 1, Subject: subject}
		return putReservationTx(tx, *result)
	}
	if !active {
		return errors.Join(task.ErrEvidenceFault, os.ErrNotExist)
	}
	return r.reserveAfterHead(tx, sequence, subject, result)
}

func (r *Resource) requireReservable(tx *taskdir.ControlTransaction) error {
	released, present, err := readOptionalReleaseTx(tx, r.request.WorkspaceID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err = validateReleaseRecord(released, r.request.WorkspaceID); err != nil {
		return classifyStoredResourceRecordError(err)
	}
	return ErrReleased
}

func (r *Resource) reserveAfterHead(tx *taskdir.ControlTransaction, sequence uint64, subject Subject, result *Reservation) error {
	if subject.Kind == SubjectAdmission {
		if err := requireOpenAdmissionSubject(tx, r.request.WorkspaceID, sequence, subject); err != nil {
			return err
		}
	}
	current, err := readReservationTx(tx, r.request.WorkspaceID, sequence)
	if err != nil {
		return requireResourceRecord(err)
	}
	closed, closeErr := readReservationClosedTx(tx, r.request.WorkspaceID, sequence)
	if task.RecordAbsent(closeErr) {
		if current.Subject == subject {
			*result = current
			return nil
		}
		return ErrBusy
	}
	if closeErr != nil {
		return closeErr
	}
	if closed.WorkspaceID != current.WorkspaceID || closed.Sequence != current.Sequence {
		return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if sequence == ^uint64(0) {
		return task.ErrEvidenceFault
	}
	*result = Reservation{SchemaVersion: task.SchemaVersion, WorkspaceID: r.request.WorkspaceID, Sequence: sequence + 1, Subject: subject}
	return putReservationTx(tx, *result)
}

func requireOpenAdmissionSubject(tx *taskdir.ControlTransaction, workspaceID string, latest uint64, subject Subject) error {
	for sequence := uint64(1); sequence <= latest; sequence++ {
		reservation, err := readReservationTx(tx, workspaceID, sequence)
		if err != nil {
			return requireResourceRecord(err)
		}
		if reservation.Subject != subject {
			continue
		}
		if closed, closeErr := readReservationClosedTx(tx, workspaceID, sequence); closeErr == nil {
			if closed.Disposition != "aborted-not-launched" {
				return task.ErrRequestConflict
			}
			continue
		} else if !task.RecordAbsent(closeErr) {
			return closeErr
		}
		return nil
	}
	return nil
}

// CloseReservation accepts only the coordinator's already-proven disposition.
// This storage method never decides whether a native lifetime is safe.
func (r *Resource) CloseReservation(ctx context.Context, reservation Reservation, disposition, captureID string) error {
	if err := r.validateReservation(reservation, reservation.Sequence); err != nil {
		return err
	}
	closed := ReservationClosed{
		SchemaVersion: task.SchemaVersion,
		WorkspaceID:   reservation.WorkspaceID,
		Sequence:      reservation.Sequence,
		Disposition:   disposition,
		CaptureID:     captureID,
	}
	if err := validateReservationClosed(closed, r.request.WorkspaceID); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		current, err := readReservationTx(tx, r.request.WorkspaceID, reservation.Sequence)
		if err != nil {
			return err
		}
		if current != reservation {
			return task.ErrIdentityMismatch
		}
		if _, err = tx.Put(reservationClosedName(reservation.Sequence), closed); err != nil {
			return err
		}
		return tx.Verify(reservationClosedName(reservation.Sequence), closed)
	})
}

func (r *Resource) validateReservation(record Reservation, sequence uint64) error {
	if sequence == 0 || record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != r.request.WorkspaceID || record.Sequence != sequence {
		return task.ErrIdentityMismatch
	}
	return validateSubject(record.Subject)
}

func readReservationTx(tx *taskdir.ControlTransaction, workspaceID string, sequence uint64) (Reservation, error) {
	if sequence == 0 {
		return Reservation{}, task.ErrIdentityMismatch
	}
	var reservation Reservation
	if err := tx.Read(reservationName(sequence), &reservation); err != nil {
		return Reservation{}, classifyResourceRecordReadError(err)
	}
	if reservation.SchemaVersion != task.SchemaVersion || reservation.WorkspaceID != workspaceID || reservation.Sequence != sequence {
		return Reservation{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := validateSubject(reservation.Subject); err != nil {
		return Reservation{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(reservationName(sequence), reservation); err != nil {
		return Reservation{}, err
	}
	return reservation, nil
}

func readReservationClosedTx(tx *taskdir.ControlTransaction, workspaceID string, sequence uint64) (ReservationClosed, error) {
	if sequence == 0 {
		return ReservationClosed{}, task.ErrIdentityMismatch
	}
	var closed ReservationClosed
	if err := tx.Read(reservationClosedName(sequence), &closed); err != nil {
		return ReservationClosed{}, classifyResourceRecordReadError(err)
	}
	if err := validateReservationClosed(closed, workspaceID); err != nil {
		return ReservationClosed{}, classifyStoredResourceRecordError(err)
	}
	if closed.Sequence != sequence {
		return ReservationClosed{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := tx.Verify(reservationClosedName(sequence), closed); err != nil {
		return ReservationClosed{}, err
	}
	return closed, nil
}

func putReservationTx(tx *taskdir.ControlTransaction, reservation Reservation) error {
	if reservation.Sequence == 0 {
		return task.ErrIdentityMismatch
	}
	if err := validateSubject(reservation.Subject); err != nil {
		return err
	}
	if _, err := tx.Put(reservationName(reservation.Sequence), reservation); err != nil {
		return err
	}
	return tx.Verify(reservationName(reservation.Sequence), reservation)
}

func validateReservationClosed(closed ReservationClosed, workspaceID string) error {
	if closed.SchemaVersion != task.SchemaVersion || closed.WorkspaceID != workspaceID || closed.Sequence == 0 {
		return task.ErrIdentityMismatch
	}
	switch closed.Disposition {
	case "captured", "unavailable", "aborted-not-launched", "operation-completed":
	default:
		return task.ErrIdentityMismatch
	}
	if closed.CaptureID != "" {
		return task.ValidateTaskID(closed.CaptureID)
	}
	return nil
}

func reservationName(sequence uint64) string {
	return fmt.Sprintf("use-%0*d.json", reservationSequenceWidth, sequence)
}

func reservationClosedName(sequence uint64) string {
	return fmt.Sprintf("use-%0*d-closed.json", reservationSequenceWidth, sequence)
}

// sequenceFromName parses only canonical active reservation names. Invalid
// family-shaped names are never treated as a missing record.
func sequenceFromName(name string) (uint64, error) {
	if !strings.HasPrefix(name, "use-") {
		return 0, os.ErrNotExist
	}
	if !strings.HasSuffix(name, ".json") {
		return 0, task.ErrEvidenceFault
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, "use-"), ".json")
	if len(stem) != reservationSequenceWidth {
		return 0, task.ErrEvidenceFault
	}
	sequence, err := strconv.ParseUint(stem, 10, 64)
	if err != nil || sequence == 0 || reservationName(sequence) != name {
		return 0, task.ErrEvidenceFault
	}
	return sequence, nil
}

func latestReservationSequence(control *taskdir.ControlDir, ctx context.Context) (uint64, bool, error) {
	sequence, present, _, err := latestReservationHead(control, ctx)
	return sequence, present, err
}

func latestReservationHead(control *taskdir.ControlDir, ctx context.Context) (uint64, bool, bool, error) {
	names, err := control.RecordNamesContext(ctx)
	if err != nil {
		return 0, false, false, err
	}
	var latest uint64
	present := false
	active := false
	for _, name := range names {
		sequence, isActive, candidate, parseErr := parseReservationRecordName(name)
		if parseErr != nil {
			return 0, false, false, parseErr
		}
		if !candidate {
			continue
		}
		if !present || sequence > latest {
			latest = sequence
			present = true
			active = isActive
		} else if sequence == latest {
			active = active || isActive
		}
	}
	return latest, present, active, nil
}

func parseReservationRecordName(name string) (uint64, bool, bool, error) {
	if !strings.HasPrefix(name, "use-") {
		return 0, false, false, nil
	}
	if !strings.HasSuffix(name, ".json") {
		return 0, false, true, task.ErrEvidenceFault
	}
	if strings.HasSuffix(name, "-closed.json") {
		stem := strings.TrimSuffix(strings.TrimPrefix(name, "use-"), "-closed.json")
		if len(stem) != reservationSequenceWidth {
			return 0, false, true, task.ErrEvidenceFault
		}
		sequence, err := strconv.ParseUint(stem, 10, 64)
		if err != nil || sequence == 0 || reservationClosedName(sequence) != name {
			return 0, false, true, task.ErrEvidenceFault
		}
		return sequence, false, true, nil
	}
	sequence, err := sequenceFromName(name)
	if err != nil {
		return 0, false, true, err
	}
	return sequence, true, true, nil
}
