package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func prepareReferenceStop(t *testing.T, td *TaskDir) string {
	t.Helper()
	id := strings.Repeat("d", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	record, err := td.ReadStopRequest(id)
	must(t, err)
	data, err := task.MarshalCanonical(record)
	must(t, err)
	return task.ComputeSHA256(data)
}

func TestUnlaunchedReferenceReturnsExistingSubmittedRefusalWithoutFencing(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	want := prepareReferenceStop(t, td)
	for range 2 {
		proven, ref, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
		if err != nil || !proven || ref != want {
			t.Fatalf("submitted proof = %v, %q, %v; want existing %q", proven, ref, err, want)
		}
	}
	stops, err := td.ReadStopRecords()
	must(t, err)
	if len(stops) != 1 || stops[0].Request.Cause != "user" {
		t.Fatalf("nonfencing proof wrote another refusal: %+v", stops)
	}
	for _, name := range []string{"provider.start", "provider.exit", "outcome.json", filepath.Join(lifetimeDirectory, lifetimeTerminationName), filepath.Join(lifetimeDirectory, lifetimeWorkerWaitName)} {
		if _, err = os.Lstat(filepath.Join(td.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("proof created %s: %v", name, err)
		}
	}
}

func TestUnlaunchedReferenceRetainsReservationForExecutionOrConflictingEvidence(t *testing.T) {
	for _, name := range []string{"raw bytes", "unselected payload", "provider start", "bad prepared identity", "bad refusal"} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			td := supervisorTask(t, store)
			if name == "provider start" {
				permit, err := td.PrepareStart(0)
				must(t, err)
				must(t, permit.Consume())
				must(t, permit.Release())
			}
			prepareReferenceStop(t, td)
			switch name {
			case "raw bytes":
				must(t, os.MkdirAll(filepath.Join(td.Dir, "raw"), 0o700))
				must(t, os.WriteFile(filepath.Join(td.Dir, "raw", "stdout"), []byte("captured bytes"), 0o600))
			case "unselected payload":
				must(t, os.WriteFile(filepath.Join(td.Dir, "result.txt"), []byte("unselected"), 0o600))
			case "bad prepared identity":
				must(t, os.WriteFile(filepath.Join(td.Dir, "meta.json"), []byte("{}"), 0o600))
			case "bad refusal":
				must(t, os.WriteFile(filepath.Join(td.Dir, "stop", strings.Repeat("d", 32)+stopRequestSuffix), []byte("{}"), 0o600))
			}
			proven, ref, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
			if proven || ref != "" {
				t.Fatalf("unsafe evidence granted proof: %v %q %v", proven, ref, err)
			}
			if (name == "bad prepared identity" || name == "bad refusal" || name == "raw bytes") && err == nil {
				t.Fatal("unreadable/invalid evidence lost its error")
			}
		})
	}
}

func TestUnlaunchedReferenceRequiresRefusalFreeOwnersAndValidContext(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	proven, ref, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
	if err != nil || proven || ref != "" {
		t.Fatalf("absence without refusal = %v %q %v", proven, ref, err)
	}
	prepareReferenceStop(t, td)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	proven, ref, err = td.ProveUnlaunchedTaskReferenceContext(t.Context())
	must(t, owner.Release())
	if err != nil || proven || ref != "" {
		t.Fatalf("held lifetime owner = %v %q %v", proven, ref, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	proven, ref, err = td.ProveUnlaunchedTaskReferenceContext(ctx)
	if proven || ref != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context = %v %q %v", proven, ref, err)
	}
}

func TestUnlaunchedReferenceDoesNotReturnProofOnUnlockFault(t *testing.T) {
	for _, path := range []string{".run.lock", ".maintenance.lock"} {
		t.Run(path, func(t *testing.T) {
			store := testStore(t)
			td := supervisorTask(t, store)
			prepareReferenceStop(t, td)
			// Injected unlock failure deliberately retains the real lease. This
			// fixture owns both descriptors and releases them after the assertion,
			// before the normal TaskDir/Store cleanup checks their ownership.
			t.Cleanup(func() {
				store.SetFaultInjector(nil)
				must(t, td.runLock.Unlock())
				must(t, store.maintLock.Unlock())
			})
			injected := errors.New("injected proof unlock failure")
			store.SetFaultInjector(&lifetimeOwnerReleaseFault{failPath: path, failures: []error{injected}})
			proven, ref, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
			if proven || ref != "" || !errors.Is(err, injected) {
				t.Fatalf("unlock failure granted proof: %v %q %v", proven, ref, err)
			}
		})
	}
}
