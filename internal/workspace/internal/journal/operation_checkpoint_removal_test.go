package journal

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func TestCheckpointRemovalRegistrationAppendsContiguousAttempts(t *testing.T) {
	store := newCheckpointStore(t)
	original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	first := openCheckpointOperation(t, store, removalRequestForTest(store, testID('2'), original))
	second := openCheckpointOperation(t, store, removalRequestForTest(store, testID('3'), original))

	attempt, created, err := original.RegisterCheckpointRemoval(t.Context(), first, 0)
	want := removalAttemptForTest(first, 1)
	if err != nil || !created || attempt != want {
		t.Fatalf("first registration=%+v created=%t err=%v", attempt, created, err)
	}
	assertCheckpointRecordBytes(t, store, original, checkpointRemovalAttemptName(1), want)
	if attempt, created, err = original.RegisterCheckpointRemoval(t.Context(), first, 0); err != nil || created || attempt != want {
		t.Fatalf("registration replay=%+v created=%t err=%v", attempt, created, err)
	}
	if _, _, err = original.RegisterCheckpointRemoval(t.Context(), second, 0); !errors.Is(err, ErrCheckpointRemovalHeadChanged) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("stale-head registration error=%v", err)
	}
	if attempt, created, err = original.RegisterCheckpointRemoval(t.Context(), second, 1); err != nil || !created || attempt != removalAttemptForTest(second, 2) {
		t.Fatalf("second registration=%+v created=%t err=%v", attempt, created, err)
	}
	attempts, err := original.CheckpointRemovalAttempts(t.Context())
	if err != nil || !slices.Equal(attempts, []contract.CheckpointRemovalReference{want, removalAttemptForTest(second, 2)}) {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
}

func TestCheckpointRemovalRegistrationRefusesUnboundRequests(t *testing.T) {
	store := newCheckpointStore(t)
	deadline := time.Now().Add(time.Hour)
	original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), deadline))
	other := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('2'), deadline))
	wrongDigest := removalRequestForTest(store, testID('3'), original)
	wrongDigest.Subject.SHA256 = testSHA('7')
	wrongCheckpoint := removalRequestForTest(store, testID('4'), other)
	verify := checkpointBaseRequest(store, testID('5'), OperationVerify, deadline)
	verify.WorkspaceID, verify.Arguments, verify.Subject = testID('6'), []string{"/bin/true"}, removalRequestForTest(store, testID('5'), original).Subject
	for name, request := range map[string]OperationRequest{"creation digest": wrongDigest, "checkpoint": wrongCheckpoint, "kind": verify} {
		removal := openCheckpointOperation(t, store, request)
		if _, _, err := original.RegisterCheckpointRemoval(t.Context(), removal, 0); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s: unbound registration error=%v", name, err)
		}
	}
	if _, _, err := original.RegisterCheckpointRemoval(t.Context(), nil, 0); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("missing removal error=%v", err)
	}
	removal := openCheckpointOperation(t, store, removalRequestForTest(store, testID('7'), original))
	if _, _, err := removal.RegisterCheckpointRemoval(t.Context(), removal, 0); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("removal registered on a removal journal: %v", err)
	}
	if attempts, err := original.CheckpointRemovalAttempts(t.Context()); err != nil || len(attempts) != 0 {
		t.Fatalf("refused registrations left attempts=%+v err=%v", attempts, err)
	}
}

// TestCheckpointRemovalConcurrentRegistrationHasOneWinner races removal
// requests through independent handles of the original journal. The shared
// operation lock admits exactly one reference at the observed head; every
// loser is refused and observes the canonical winner.
func TestCheckpointRemovalConcurrentRegistrationHasOneWinner(t *testing.T) {
	store := newCheckpointStore(t)
	request := ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour))
	original := openCheckpointOperation(t, store, request)
	const contenders = 6
	removals := make([]*Operation, contenders)
	handles := make([]*Operation, contenders)
	for index := range contenders {
		removals[index] = openCheckpointOperation(t, store, removalRequestForTest(store, testID(byte('7'+index)), original))
		handle, err := LoadOperation(t.Context(), store, request.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeOperation(t, handle) })
		handles[index] = handle
	}
	start := make(chan struct{})
	errs := make([]error, contenders)
	created := make([]bool, contenders)
	var wait sync.WaitGroup
	for index := range contenders {
		wait.Go(func() {
			<-start
			_, created[index], errs[index] = handles[index].RegisterCheckpointRemoval(t.Context(), removals[index], 0)
		})
	}
	close(start)
	wait.Wait()
	winner := -1
	for index := range contenders {
		switch {
		case errs[index] == nil && created[index] && winner < 0:
			winner = index
		case !errors.Is(errs[index], ErrCheckpointRemovalHeadChanged):
			t.Fatalf("contender %d created=%t err=%v", index, created[index], errs[index])
		}
	}
	attempts, err := original.CheckpointRemovalAttempts(t.Context())
	if winner < 0 || err != nil || !slices.Equal(attempts, []contract.CheckpointRemovalReference{removalAttemptForTest(removals[winner], 1)}) {
		t.Fatalf("winner=%d attempts=%+v err=%v", winner, attempts, err)
	}
}

func TestCheckpointRemovalAmbiguousRegistrationReplaysSameRecord(t *testing.T) {
	store := newCheckpointStore(t)
	original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	removal := openCheckpointOperation(t, store, removalRequestForTest(store, testID('2'), original))
	fault := &postLinkBarrierFault{name: checkpointRemovalAttemptName(1)}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	if _, _, err := original.RegisterCheckpointRemoval(t.Context(), removal, 0); !errors.Is(err, task.ErrUncertainDurability) || !fault.fired {
		t.Fatalf("ambiguous registration error=%v fired=%t", err, fault.fired)
	}
	want := removalAttemptForTest(removal, 1)
	attempt, created, err := original.RegisterCheckpointRemoval(t.Context(), removal, 0)
	if err != nil || created || attempt != want {
		t.Fatalf("recovered registration=%+v created=%t err=%v", attempt, created, err)
	}
	later := openCheckpointOperation(t, store, removalRequestForTest(store, testID('3'), original))
	if _, _, err = original.RegisterCheckpointRemoval(t.Context(), later, 0); !errors.Is(err, ErrCheckpointRemovalHeadChanged) {
		t.Fatalf("registration over a recovered head error=%v", err)
	}
}

func TestCheckpointRemovalInventoryRefusesCorruptAttempts(t *testing.T) {
	for name, seed := range corruptRemovalInventories() {
		t.Run(name, func(t *testing.T) {
			store := newCheckpointStore(t)
			original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
			removal := openCheckpointOperation(t, store, removalRequestForTest(store, testID('2'), original))
			seed(t, original, removal)
			if _, err := original.CheckpointRemovalAttempts(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("corrupt inventory read error=%v", err)
			}
			later := openCheckpointOperation(t, store, removalRequestForTest(store, testID('3'), original))
			if _, _, err := original.RegisterCheckpointRemoval(t.Context(), later, 1); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("registration over corrupt inventory error=%v", err)
			}
		})
	}
}

// corruptRemovalInventories seeds raw removal-namespace records that no
// registration can produce: gaps, noncanonical or overflowing names, foreign
// families, unregistered receipts and invalid attempt lists.
func corruptRemovalInventories() map[string]func(*testing.T, *Operation, *Operation) {
	return map[string]func(*testing.T, *Operation, *Operation){
		"gap": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, checkpointRemovalAttemptName(2), removalAttemptForTest(removal, 2))
		},
		"unpadded name": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, "removal-attempt-1.json", removalAttemptForTest(removal, 1))
		},
		"overflowing name": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, "removal-attempt-18446744073709551616.json", removalAttemptForTest(removal, 1))
		},
		"maximum sequence": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, "removal-attempt-18446744073709551615.json", removalAttemptForTest(removal, ^uint64(0)))
		},
		"unknown family": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, "removal-current.json", removalAttemptForTest(removal, 1))
		},
		"orphan receipt": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, checkpointRemovalReceiptName(1), removalReceiptForTest(removal, 1, contract.CheckpointNothingToRemove))
		},
		"sequence mismatch": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, checkpointRemovalAttemptName(1), removalAttemptForTest(removal, 2))
		},
		"foreign checkpoint": func(t *testing.T, original, removal *Operation) {
			attempt := removalAttemptForTest(removal, 1)
			attempt.CheckpointID = testID('9')
			seedRemovalRecord(t, original, checkpointRemovalAttemptName(1), attempt)
		},
		"duplicate operation": func(t *testing.T, original, removal *Operation) {
			seedRemovalRecord(t, original, checkpointRemovalAttemptName(1), removalAttemptForTest(removal, 1))
			seedRemovalRecord(t, original, checkpointRemovalAttemptName(2), removalAttemptForTest(removal, 2))
		},
	}
}

func TestCheckpointRemovalReceiptsAuthenticateRegisteredAttempts(t *testing.T) {
	store := newCheckpointStore(t)
	original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	registered := openCheckpointOperation(t, store, removalRequestForTest(store, testID('2'), original))
	unregistered := openCheckpointOperation(t, store, removalRequestForTest(store, testID('3'), original))
	if _, _, err := original.RegisterCheckpointRemoval(t.Context(), registered, 0); err != nil {
		t.Fatal(err)
	}
	forged := removalReceiptForTest(unregistered, 1, contract.CheckpointNothingToRemove)
	if err := original.RecordCheckpointRemovalReceipt(t.Context(), unregistered, forged); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unregistered receipt error=%v", err)
	}
	wrongDigest := removalReceiptForTest(registered, 1, contract.CheckpointNothingToRemove)
	wrongDigest.Attempt.RequestSHA256 = testSHA('7')
	if err := original.RecordCheckpointRemovalReceipt(t.Context(), registered, wrongDigest); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("wrong-digest receipt error=%v", err)
	}
	if err := original.RecordCheckpointRemovalReceipt(t.Context(), unregistered, removalReceiptForTest(registered, 1, contract.CheckpointNothingToRemove)); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("receipt through another removal error=%v", err)
	}
	if _, err := original.CheckpointRemovalReceipt(t.Context(), forged.Attempt); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unregistered receipt read error=%v", err)
	}
	if _, err := original.CheckpointRemovalReceipt(t.Context(), removalAttemptForTest(registered, 1)); !task.RecordAbsent(err) {
		t.Fatalf("unclosed attempt receipt error=%v, want absent", err)
	}
}

// TestCheckpointRemovalFailedAttemptDoesNotBlockLaterSuccess runs after the
// original lifetime is closed: registration and receipts need no launch
// permission, a failed attempt occupies only its own sequence, and the
// original result is never written.
func TestCheckpointRemovalFailedAttemptDoesNotBlockLaterSuccess(t *testing.T) {
	store := newCheckpointStore(t)
	original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(-time.Second)))
	if err := original.RecordCheckpointCandidate(t.Context(), checkpointCandidateForTest(t, original)); err != nil {
		t.Fatal(err)
	}
	if _, err := original.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := openCheckpointOperation(t, store, removalRequestForTest(store, testID('2'), original))
	second := openCheckpointOperation(t, store, removalRequestForTest(store, testID('3'), original))
	if _, _, err := original.RegisterCheckpointRemoval(t.Context(), first, 0); err != nil {
		t.Fatal(err)
	}
	failed := removalReceiptForTest(first, 1, contract.CheckpointUnavailable)
	failed.ReasonCode = contract.CheckpointRemovalFailed
	recordRemovalReceipt(t, original, first, failed)
	if _, _, err := original.RegisterCheckpointRemoval(t.Context(), second, 1); err != nil {
		t.Fatalf("fresh attempt after a closed failure refused: %v", err)
	}
	removed := removalReceiptForTest(second, 2, contract.CheckpointRemoved)
	recordRemovalReceipt(t, original, second, removed)
	assertCheckpointRecordBytes(t, store, original, checkpointRemovalReceiptName(2), removed)
	retried := removalReceiptForTest(first, 1, contract.CheckpointRemoved)
	if err := original.RecordCheckpointRemovalReceipt(t.Context(), first, retried); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("failed attempt receipt was replaced: %v", err)
	}
	if _, err := original.Result(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("removal wrote the original result: %v", err)
	}
}

func TestCheckpointRemovalWithoutCandidateCannotRecordRemoved(t *testing.T) {
	store := newCheckpointStore(t)
	original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	removal := openCheckpointOperation(t, store, removalRequestForTest(store, testID('2'), original))
	if _, _, err := original.RegisterCheckpointRemoval(t.Context(), removal, 0); err != nil {
		t.Fatal(err)
	}
	removed := removalReceiptForTest(removal, 1, contract.CheckpointRemoved)
	if err := original.RecordCheckpointRemovalReceipt(t.Context(), removal, removed); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("removed without candidate error=%v", err)
	}
	if _, err := original.control.PutContext(t.Context(), checkpointRemovalReceiptName(1), removed); err != nil {
		t.Fatal(err)
	}
	if _, err := original.CheckpointRemovalReceipt(t.Context(), removed.Attempt); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("stored removed without candidate error=%v", err)
	}

	clean := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('3'), time.Now().Add(time.Hour)))
	cleanRemoval := openCheckpointOperation(t, store, removalRequestForTest(store, testID('4'), clean))
	if _, _, err := clean.RegisterCheckpointRemoval(t.Context(), cleanRemoval, 0); err != nil {
		t.Fatal(err)
	}
	recordRemovalReceipt(t, clean, cleanRemoval, removalReceiptForTest(cleanRemoval, 1, contract.CheckpointNothingToRemove))
}

func recordRemovalReceipt(t *testing.T, original, removal *Operation, receipt contract.CheckpointRemovalReceipt) {
	t.Helper()
	if err := original.RecordCheckpointRemovalReceipt(t.Context(), removal, receipt); err != nil {
		t.Fatal(err)
	}
	if err := original.RecordCheckpointRemovalReceipt(t.Context(), removal, receipt); err != nil {
		t.Fatalf("matching removal receipt replay failed: %v", err)
	}
	if stored, err := original.CheckpointRemovalReceipt(t.Context(), receipt.Attempt); err != nil || !reflect.DeepEqual(stored, receipt) {
		t.Fatalf("stored removal receipt=%+v err=%v", stored, err)
	}
}

func removalRequestForTest(store *taskdir.Store, operationID string, original *Operation) OperationRequest {
	request := checkpointBaseRequest(store, operationID, contract.OperationCheckpointRemove, time.Now().Add(time.Hour))
	request.Subject = Subject{Kind: SubjectOperation, ID: original.Request().OperationID, SHA256: original.Digest()}
	return request
}

func removalAttemptForTest(removal *Operation, sequence uint64) contract.CheckpointRemovalReference {
	request := removal.Request()
	return contract.CheckpointRemovalReference{
		SchemaVersion: task.SchemaVersion, CheckpointID: request.Subject.ID, Sequence: sequence, OperationID: request.OperationID, RequestSHA256: removal.Digest(),
	}
}

func removalReceiptForTest(removal *Operation, sequence uint64, status string) contract.CheckpointRemovalReceipt {
	return contract.CheckpointRemovalReceipt{
		SchemaVersion: task.SchemaVersion, Attempt: removalAttemptForTest(removal, sequence), Status: status,
		TerminationSHA256: testSHA('1'), WorkerWaitSHA256: testSHA('2'),
	}
}

func seedRemovalRecord[Record any](t *testing.T, original *Operation, name string, record Record) {
	t.Helper()
	if _, err := original.control.PutContext(t.Context(), name, record); err != nil {
		t.Fatal(err)
	}
}
