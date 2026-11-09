package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPassiveLastObservationUsesCanonicalOwnerValidation(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	intent := testIntent()
	intent.OriginRootID = store.RootID
	reference, err := OpenReference(t.Context(), store, intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reference.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = reference.SaveIntent(t.Context(), intent, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	response := responseFor(Request{})
	response.Identity = intent.Destination
	if err = reference.Observe(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	observation, err := ReadLastObservation(t.Context(), reader, intent.TaskID, intent)
	if err != nil || observation.Sequence != 1 {
		t.Fatalf("passive cached observation: %+v, %v", observation, err)
	}
	name := filepath.Join(store.Root, "remote-refs", intent.TaskID, "observation-00000000000000000001.json")
	file, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write([]byte(" "))
	if err = errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadLastObservation(t.Context(), reader, intent.TaskID, intent); err == nil {
		t.Fatal("noncanonical cached observation accepted")
	}
}

func observationCacheNames(t *testing.T, reference *Reference) []string {
	t.Helper()
	names, err := reference.records.RecordNamesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result := []string{}
	for _, name := range names {
		if _, ok := observationSequence(name); ok {
			result = append(result, name)
		}
	}
	return result
}

func TestObservationCacheRetainsOnlyNewestTwo(t *testing.T) {
	reference, _ := testReference(t)
	if err := reference.SaveControl(t.Context(), Cancel, Control{ID: strings.Repeat("d", 32)}); err != nil {
		t.Fatal(err)
	}
	response := responseFor(Request{})
	for index := 1; index <= 32; index++ {
		response.Envelope = []byte(fmt.Sprintf(`{"observation":%d}`, index))
		if err := reference.Observe(t.Context(), response); err != nil {
			t.Fatal(err)
		}
		names := observationCacheNames(t, reference)
		if len(names) != min(index, retainedObservations) {
			t.Fatalf("unbounded cache at %d: %v", index, names)
		}
	}
	latest, err := reference.LastObservation(t.Context())
	if err != nil || latest.Sequence != 32 || string(latest.Response.Envelope) != `{"observation":32}` {
		t.Fatalf("latest cache lost: %+v %v", latest, err)
	}
	intent, err := reference.Intent()
	if err != nil || intent.TaskID != testIntent().TaskID {
		t.Fatalf("intent pruned: %+v %v", intent, err)
	}
	var control operationRecord
	if err = reference.records.Read("control-"+strings.Repeat("d", 32)+".json", &control); err != nil {
		t.Fatalf("control pruned: %v", err)
	}
}

type observationPruneFault struct {
	taskdir.BaseFaultInjector
	failure error
}

func (f *observationPruneFault) OnCleanupUnlink(path string) error {
	if strings.HasPrefix(filepath.Base(path), "observation-") {
		return f.failure
	}
	return nil
}

func TestObservationCacheInterruptedPrunePreservesNewestAndBoundsGrowth(t *testing.T) {
	reference, store := testReference(t)
	response := responseFor(Request{})
	for range 2 {
		if err := reference.Observe(t.Context(), response); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("cache prune interrupted")
	store.SetFaultInjector(&observationPruneFault{failure: failure})
	response.Envelope = []byte(`{"latest":true}`)
	for range 3 {
		if err := reference.Observe(t.Context(), response); !errors.Is(err, failure) {
			t.Fatalf("interrupted prune hidden: %v", err)
		}
		if count := len(observationCacheNames(t, reference)); count != 3 {
			t.Fatalf("cache grew after interrupted prune: %d", count)
		}
	}
	latest, err := reference.LastObservation(t.Context())
	if err != nil || latest.Sequence != 3 || string(latest.Response.Envelope) != `{"latest":true}` {
		t.Fatalf("newest durable cache lost: %+v %v", latest, err)
	}
	store.SetFaultInjector(nil)
	if err = reference.Observe(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	if count := len(observationCacheNames(t, reference)); count != 2 {
		t.Fatalf("cache did not recover bound: %d", count)
	}
	latest, err = reference.LastObservation(t.Context())
	if err != nil || latest.Sequence != 4 {
		t.Fatalf("recovered sequence: %+v %v", latest, err)
	}
}

func TestLastObservationDoesNotReadHistoricalPayloads(t *testing.T) {
	reference, _ := testReference(t)
	response := responseFor(Request{})
	for sequence := uint64(1); sequence <= 3; sequence++ {
		record := Observation{Sequence: sequence, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Response: response}
		if _, err := reference.records.PutContext(t.Context(), fmt.Sprintf("observation-%020d.json", sequence), record); err != nil {
			t.Fatal(err)
		}
	}
	// An older unrecognized cache payload is retained, but not consulted to read
	// the latest verified cache. A writer must refuse to delete this unknown data.
	if _, err := reference.records.PutContext(t.Context(), "observation-00000000000000000000.json", struct {
		Unknown string `json:"unknown"`
	}{Unknown: "retain"}); err != nil {
		t.Fatal(err)
	}
	latest, err := reference.LastObservation(t.Context())
	if err != nil || latest.Sequence != 3 {
		t.Fatalf("historical payload affected latest: %+v %v", latest, err)
	}
	if err = reference.Observe(t.Context(), response); err == nil {
		t.Fatal("writer deleted unknown old cache payload")
	}
	names, err := reference.records.RecordNamesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range names {
		if name == "observation-00000000000000000000.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("unknown cache record deleted")
	}
}

func TestObservationCacheMissingIsNotTaskAbsence(t *testing.T) {
	reference, _ := testReference(t)
	if _, err := reference.LastObservation(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cache: %v", err)
	}
	if _, err := reference.Intent(); err != nil {
		t.Fatalf("cache absence changed intent: %v", err)
	}
}

func TestOperationErrorKeepsAcceptanceUnknownIdentity(t *testing.T) {
	err := &OperationError{Code: "acceptance_unknown", Message: "original request may have been accepted"}
	if !errors.Is(fmt.Errorf("observing: %w", err), ErrAcceptanceUnknown) {
		t.Fatal("operation envelope lost uncertainty marker")
	}
	if errors.Is(&OperationError{Code: "request_conflict"}, ErrAcceptanceUnknown) {
		t.Fatal("definite conflict misclassified unknown")
	}
}

func TestCorruptObservationCacheDoesNotBlockAuthoritativeOperations(t *testing.T) {
	for _, operation := range []Operation{Admit, Reconcile, Status, Cancel} {
		t.Run(string(operation), func(t *testing.T) {
			reference, _ := testReference(t)
			const corrupt = "observation-00000000000000000001.json"
			if _, err := reference.records.PutContext(t.Context(), corrupt, struct {
				Unknown bool `json:"unknown"`
			}{true}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
				calls++
				response := responseFor(request)
				response.TaskRecord = "created"
				return response, nil
			})}
			for range 3 {
				response, err := runCacheFaultOperation(t.Context(), coordinator, reference, operation)
				assertCacheFaultPreservesRemoteResult(t, response, err)
			}
			if calls != 3 {
				t.Fatalf("authoritative exchange calls=%d", calls)
			}
			assertCorruptCachePreservesAuthority(t, reference, operation, corrupt)
		})
	}
}

func assertCacheFaultPreservesRemoteResult(t *testing.T, response Response, err error) {
	t.Helper()
	if err != nil || response.TaskRecord != "created" || !response.Existing {
		t.Fatalf("cache fault replaced destination result: %+v %v", response, err)
	}
	if len(response.Diagnostics) != 1 || response.Diagnostics[0] != "observation_cache_write_failed" {
		t.Fatalf("cache failure hidden: %v", response.Diagnostics)
	}
}

func assertCorruptCachePreservesAuthority(t *testing.T, reference *Reference, operation Operation, corrupt string) {
	t.Helper()
	names := observationCacheNames(t, reference)
	if len(names) != 1 || names[0] != corrupt {
		t.Fatalf("cache grew or corrupt evidence was deleted: %v", names)
	}
	if operation == Admit || operation == Reconcile {
		if _, err := reference.Accepted(); err != nil {
			t.Fatalf("verified admission was not retained: %v", err)
		}
	}
}

func runCacheFaultOperation(ctx context.Context, coordinator Coordinator, reference *Reference, operation Operation) (Response, error) {
	if operation == Admit {
		return coordinator.Dispatch(ctx, reference)
	}
	if operation == Reconcile {
		return coordinator.Reconcile(ctx, reference)
	}
	if operation == Status {
		return coordinator.Observe(ctx, reference, operation, 0, 0)
	}
	return coordinator.Control(ctx, reference, operation, Control{ID: strings.Repeat("d", 32)})
}
