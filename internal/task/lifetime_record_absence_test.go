package task

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
)

// absenceMarkerError is an opaque leaf that legitimately reports absence through
// its own Is method.
type absenceMarkerError struct{}

func (absenceMarkerError) Error() string        { return "marked absent" }
func (absenceMarkerError) Is(target error) bool { return target == fs.ErrNotExist }

// absenceMaskingError advertises absence while its actual cause is a fault.
type absenceMaskingError struct {
	cause error
}

func (wrapper absenceMaskingError) Error() string        { return "masked: " + wrapper.cause.Error() }
func (wrapper absenceMaskingError) Is(target error) bool { return target == fs.ErrNotExist }
func (wrapper absenceMaskingError) Unwrap() error        { return wrapper.cause }

// contextMarkedAbsenceError advertises a whole-error marker while unwrapping
// only to a real absence, so leaf traversal alone cannot refuse it.
type contextMarkedAbsenceError struct {
	marker error
	cause  error
}

func (wrapper contextMarkedAbsenceError) Error() string {
	return wrapper.marker.Error() + ": " + wrapper.cause.Error()
}
func (wrapper contextMarkedAbsenceError) Is(target error) bool { return target == wrapper.marker }
func (wrapper contextMarkedAbsenceError) Unwrap() error        { return wrapper.cause }

func TestRecordAbsentRequiresEveryCauseToBeAbsence(t *testing.T) {
	_, missing := fs.Stat(os.DirFS(t.TempDir()), "record.json")
	if !errors.Is(missing, fs.ErrNotExist) {
		t.Fatalf("missing record stat = %v, want filesystem absence", missing)
	}
	ioFault := errors.New("input/output error")
	unlock := &fs.PathError{Op: "flock", Path: ".operation.lock", Err: ioFault}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "sentinel", err: fs.ErrNotExist, want: true},
		{name: "path error", err: missing, want: true},
		{name: "wrapped", err: fmt.Errorf("read release: %w", missing), want: true},
		{name: "join of absences", err: errors.Join(fs.ErrNotExist, missing), want: true},
		{name: "nested join of absences", err: errors.Join(missing, fmt.Errorf("nested: %w", errors.Join(fs.ErrNotExist, missing))), want: true},
		{name: "opaque custom absence", err: fmt.Errorf("marker: %w", absenceMarkerError{}), want: true},
		{name: "absence joined with I/O fault", err: errors.Join(missing, ioFault)},
		{name: "absence joined with unlock path error", err: errors.Join(missing, unlock)},
		{name: "absence joined with closed file", err: errors.Join(fs.ErrNotExist, os.ErrClosed)},
		{name: "absence joined with permission", err: errors.Join(fs.ErrNotExist, fs.ErrPermission)},
		{name: "wrapped absence joined with I/O fault", err: fmt.Errorf("transaction: %w", errors.Join(missing, ioFault))},
		{name: "nested mixed join", err: errors.Join(missing, fmt.Errorf("nested: %w", errors.Join(fs.ErrNotExist, ioFault)))},
		{name: "wrapper masking I/O fault", err: absenceMaskingError{cause: ioFault}},
		{name: "empty join", err: errors.Join(nil)},
		{name: "identity wrapper", err: fmt.Errorf("%w: %w", ErrIdentityMismatch, missing)},
		{name: "evidence wrapper", err: fmt.Errorf("%w: %w", ErrEvidenceFault, missing)},
		{name: "canceled join", err: errors.Join(missing, context.Canceled)},
		{name: "deadline wrapper", err: fmt.Errorf("%w: %w", context.DeadlineExceeded, fs.ErrNotExist)},
		{name: "identity in nested absence join", err: errors.Join(fs.ErrNotExist, fmt.Errorf("dependency: %w", errors.Join(ErrIdentityMismatch, missing)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecordAbsent(tc.err); got != tc.want {
				t.Fatalf("RecordAbsent(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestRecordAbsentRefusesMarkedWrapperBeforeLeafTraversal(t *testing.T) {
	_, missing := fs.Stat(os.DirFS(t.TempDir()), "record.json")
	absence := func(cause error) bool { return errors.Is(cause, fs.ErrNotExist) }
	for _, tc := range []struct {
		name   string
		marker error
	}{
		{name: "identity", marker: ErrIdentityMismatch},
		{name: "evidence", marker: ErrEvidenceFault},
		{name: "canceled", marker: context.Canceled},
		{name: "deadline", marker: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := contextMarkedAbsenceError{marker: tc.marker, cause: missing}
			if !errors.Is(err, tc.marker) || !errors.Is(err, fs.ErrNotExist) || !AllErrorCauses(err, absence) {
				t.Fatalf("fixture %v does not carry the marker over a real absence leaf", err)
			}
			if RecordAbsent(err) {
				t.Fatalf("RecordAbsent(%v) = true, want false", err)
			}
		})
	}
}
