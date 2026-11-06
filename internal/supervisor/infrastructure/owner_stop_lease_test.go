package infrastructure

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOwnerStopLeaseProbeDoesNotCreateOrTreatAbsenceAsFree(t *testing.T) {
	path := leasePath(t)
	called := false
	err := probeExistingLease(t.Context(), path, func() error { called = true; return nil })
	if called || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing lease became free: observed=%t error=%v", called, err)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive probe created an election inode: %v", err)
	}
}

func TestOwnerStopLeaseProbeHoldsExactExistingInodeThroughObservation(t *testing.T) {
	path := leasePath(t)
	lease := acquire(t, path)
	called := false
	err := probeExistingLease(t.Context(), path, func() error { called = true; return nil })
	if err != nil || called {
		t.Fatalf("held lease invoked observation: observed=%t error=%v", called, err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	observedErr := errors.New("read-only observation failed")
	err = probeExistingLease(t.Context(), path, func() error {
		called = true
		contender, acquired, cause := TryAcquireLease(t.Context(), path)
		if contender != nil {
			cause = errors.Join(cause, contender.Close())
		}
		if acquired || cause != nil {
			t.Errorf("probe did not retain exclusion: acquired=%t error=%v", acquired, cause)
		}
		return observedErr
	})
	if !called || !errors.Is(err, observedErr) {
		t.Fatalf("probe lost observation failure: observed=%t error=%v", called, err)
	}
	assertAvailable(t, path, true)
}

func TestOwnerStopLeaseProbeRejectsReplacementDuringObservation(t *testing.T) {
	path := leasePath(t)
	lease := acquire(t, path)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	err := probeExistingLease(t.Context(), path, func() error {
		if renameErr := os.Rename(path, filepath.Join(filepath.Dir(path), "retained.lock")); renameErr != nil {
			return renameErr
		}
		return os.WriteFile(path, nil, 0o600)
	})
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed lease path accepted: %v", err)
	}
}
