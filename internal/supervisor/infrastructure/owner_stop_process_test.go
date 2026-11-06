package infrastructure

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

// Decode the consumer's JSON contract so absence is distinguishable from a
// zero-valued identity and this regression can run before the projection exists.
func ownerStopJSONProcess(t *testing.T, result OwnerStopResult) *ProcessIdentity {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Process *ProcessIdentity `json:"process"`
	}
	if err = json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	return response.Process
}

func TestOwnerStopProcessProjectionIdentifiesReleasedWaiter(t *testing.T) {
	f := drainedServingReleaseFixture(t)
	reader := contextPassiveReader(t, f.entry.journal)
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	expected := ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()}
	if released, releaseErr := f.entry.releaseServingContext(t.Context()); releaseErr != nil || !released {
		t.Fatalf("owned serving release: %t %v", released, releaseErr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := StopOwnerServing(ctx, reader)
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending {
		t.Fatalf("retired live waiter: %+v %v", result, err)
	}
	process := ownerStopJSONProcess(t, result)
	if process == nil || *process != expected {
		t.Fatalf("released waiter identity missing or changed: got=%v want=%+v", process, expected)
	}
}

func TestOwnerStopProcessProjectionPreservesHistoricalIncarnation(t *testing.T) {
	f := newOwnerContactFixture(t)
	selectAbsentOwnerStopIncarnation(t, f)
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := StopOwnerServing(ctx, f.reader)
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessClosed {
		t.Fatalf("absent historical incarnation: %+v %v", result, err)
	}
	process := ownerStopJSONProcess(t, result)
	if process == nil || *process != f.auth.assumption.Process {
		t.Fatalf("historical token replaced by current PID occupant: got=%v want=%+v", process, f.auth.assumption.Process)
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestOwnerStopProcessProjectionOmitsUnselectedOwner(t *testing.T) {
	for _, residue := range []bool{false, true} {
		t.Run(map[bool]string{false: "uninitialized", true: "unknown"}[residue], func(t *testing.T) {
			reader := newOwnerStopEmptyReader(t)
			if residue {
				createOwnerStopResidue(t, reader.Root(), "empty lease")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := StopOwnerServing(ctx, reader)
			if residue && (err == nil || result.State != OwnerStopUnknown) {
				t.Fatalf("unvalidated owner became absence: %+v %v", result, err)
			}
			if !residue && (err != nil || result.State != OwnerNotInitialized) {
				t.Fatalf("uninitialized root: %+v %v", result, err)
			}
			if process := ownerStopJSONProcess(t, result); process != nil {
				t.Fatalf("unselected owner acquired process identity: %+v", process)
			}
		})
	}
}
