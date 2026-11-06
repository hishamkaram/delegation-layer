package infrastructure

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type contactPinSequence struct {
	store  *taskdir.ReadOnlyStore
	calls  int
	before func(int) error
}

func (s *contactPinSequence) PinRootContext(ctx context.Context) (taskdir.RootPin, error) {
	s.calls++
	if s.before != nil {
		if err := s.before(s.calls); err != nil {
			return taskdir.RootPin{}, err
		}
	}
	return s.store.PinRootContext(ctx)
}

func TestOwnerClosedEndpointRequiresFinalNativeRootPin(t *testing.T) {
	f := newOwnerContactFixture(t)
	if err := os.Remove(f.auth.publication.Endpoint.Path); err != nil {
		t.Fatal(err)
	}
	moved := f.auth.root + "-moved"
	replaced := false
	t.Cleanup(func() {
		if replaced {
			if err := os.Remove(f.auth.root); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Error(err)
				return
			}
			if err := os.Rename(moved, f.auth.root); err != nil {
				t.Error(err)
			}
		}
	})
	observer := &contactPinSequence{store: f.reader, before: func(call int) error {
		if call != 2 {
			return nil
		}
		if err := os.Rename(f.auth.root, moved); err != nil {
			return err
		}
		replaced = true
		return os.Mkdir(f.auth.root, 0o700)
	}}
	err := checkClosedOwnerContactEndpoint(t.Context(), observer, f.journal.pin, f.auth.publication.Endpoint)
	if err == nil || observer.calls != 2 {
		t.Fatalf("closed endpoint skipped final native root observation: calls=%d err=%v", observer.calls, err)
	}
}

func TestOwnerClosedEndpointDoesNotTreatPinAbsenceAsSocketAbsence(t *testing.T) {
	f := newOwnerContactFixture(t)
	moved := f.auth.root + "-moved"
	if err := os.Rename(f.auth.root, moved); err != nil {
		t.Fatal(err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			if err := os.Rename(moved, f.auth.root); err != nil {
				t.Error(err)
			}
		}
	})
	observer := &contactPinSequence{store: f.reader, before: func(call int) error {
		if call == 2 {
			if err := os.Rename(moved, f.auth.root); err != nil {
				return err
			}
			restored = true
		}
		return nil
	}}
	err := checkClosedOwnerContactEndpoint(t.Context(), observer, f.journal.pin, f.auth.publication.Endpoint)
	if !errors.Is(err, os.ErrNotExist) || observer.calls != 1 {
		t.Fatalf("root absence was mistaken for endpoint closure: calls=%d err=%v", observer.calls, err)
	}
}
