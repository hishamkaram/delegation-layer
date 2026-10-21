package contract

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func removalReference(sequence uint64, operationID string) CheckpointRemovalReference {
	return CheckpointRemovalReference{SchemaVersion: task.SchemaVersion, CheckpointID: repeatHex('2', 32), Sequence: sequence, OperationID: operationID, RequestSHA256: candidateRequestSHA256}
}

func TestCheckpointRemovalReferenceBindsExactAttempt(t *testing.T) {
	request := removalRequest()
	if err := ValidateCheckpointRemovalReference(removalReference(1, request.OperationID), request, candidateRequestSHA256); err != nil {
		t.Fatalf("valid reference rejected: %v", err)
	}
	cases := map[string]func(*CheckpointRemovalReference, *OperationRequest){
		"other checkpoint": func(reference *CheckpointRemovalReference, _ *OperationRequest) {
			reference.CheckpointID = repeatHex('3', 32)
		},
		"other operation": func(reference *CheckpointRemovalReference, _ *OperationRequest) {
			reference.OperationID = repeatHex('b', 32)
		},
		"other digest": func(reference *CheckpointRemovalReference, _ *OperationRequest) {
			reference.RequestSHA256 = repeatHex('9', 64)
		},
		"zero sequence":    func(reference *CheckpointRemovalReference, _ *OperationRequest) { reference.Sequence = 0 },
		"schema":           func(reference *CheckpointRemovalReference, _ *OperationRequest) { reference.SchemaVersion = 0 },
		"creation request": func(_ *CheckpointRemovalReference, request *OperationRequest) { request.Kind = OperationCheckpoint },
		"task subject":     func(_ *CheckpointRemovalReference, request *OperationRequest) { request.Subject.Kind = SubjectTask },
		"malformed checkpoint": func(reference *CheckpointRemovalReference, request *OperationRequest) {
			reference.CheckpointID, request.Subject.ID = "checkpoint", "checkpoint"
		},
	}
	for name, mutate := range cases {
		reference, mismatched := removalReference(1, request.OperationID), removalRequest()
		mutate(&reference, &mismatched)
		if err := ValidateCheckpointRemovalReference(reference, mismatched, candidateRequestSHA256); err == nil {
			t.Fatalf("%s: mismatched reference accepted", name)
		}
	}
}

func TestCheckpointRemovalReferencesAreContiguousFromOne(t *testing.T) {
	checkpointID := repeatHex('2', 32)
	for _, empty := range [][]CheckpointRemovalReference{nil, {}} {
		for _, invalidID := range []string{"", "checkpoint", repeatHex('A', 32), repeatHex('2', 31)} {
			if err := ValidateCheckpointRemovalReferences(invalidID, empty); !errors.Is(err, task.ErrInvalidTaskID) {
				t.Fatalf("invalid checkpoint ID %q with %#v error=%v, want invalid task ID", invalidID, empty, err)
			}
		}
	}
	valid := [][]CheckpointRemovalReference{
		nil,
		{},
		{removalReference(1, repeatHex('a', 32))},
		{removalReference(1, repeatHex('a', 32)), removalReference(2, repeatHex('b', 32)), removalReference(3, repeatHex('c', 32))},
	}
	for _, references := range valid {
		if err := ValidateCheckpointRemovalReferences(checkpointID, references); err != nil {
			t.Fatalf("contiguous references %+v rejected: %v", references, err)
		}
	}
	foreign := removalReference(2, repeatHex('b', 32))
	foreign.CheckpointID = repeatHex('3', 32)
	malformed := removalReference(2, repeatHex('b', 32))
	malformed.RequestSHA256 = "digest"
	invalid := map[string][]CheckpointRemovalReference{
		"starts after one":    {removalReference(2, repeatHex('a', 32))},
		"gap":                 {removalReference(1, repeatHex('a', 32)), removalReference(3, repeatHex('b', 32))},
		"duplicate sequence":  {removalReference(1, repeatHex('a', 32)), removalReference(1, repeatHex('b', 32))},
		"out of order":        {removalReference(2, repeatHex('a', 32)), removalReference(1, repeatHex('b', 32))},
		"duplicate operation": {removalReference(1, repeatHex('a', 32)), removalReference(2, repeatHex('a', 32))},
		"foreign checkpoint":  {removalReference(1, repeatHex('a', 32)), foreign},
		"malformed digest":    {removalReference(1, repeatHex('a', 32)), malformed},
	}
	for name, references := range invalid {
		if err := ValidateCheckpointRemovalReferences(checkpointID, references); err == nil {
			t.Fatalf("%s references accepted", name)
		}
	}
}

func TestNextCheckpointRemovalSequenceRefusesOverflow(t *testing.T) {
	for last, want := range map[uint64]uint64{0: 1, 1: 2, ^uint64(0) - 1: ^uint64(0)} {
		if got, err := NextCheckpointRemovalSequence(last); err != nil || got != want {
			t.Fatalf("NextCheckpointRemovalSequence(%d)=%d, %v; want %d", last, got, err, want)
		}
	}
	if got, err := NextCheckpointRemovalSequence(^uint64(0)); !errors.Is(err, task.ErrIdentityMismatch) || got != 0 {
		t.Fatalf("overflow=%d, %v; want refusal", got, err)
	}
}

func removalReceipt(attempt CheckpointRemovalReference) CheckpointRemovalReceipt {
	closure := launchedClosure()
	return CheckpointRemovalReceipt{
		SchemaVersion: task.SchemaVersion, Attempt: attempt, Status: CheckpointRemoved,
		TerminationSHA256: closure.TerminationSHA256, WorkerWaitSHA256: closure.WorkerWaitSHA256,
	}
}

func TestCheckpointRemovalReceiptDistinguishesTerminalOutcomes(t *testing.T) {
	attempt := removalReference(2, repeatHex('b', 32))
	valid := map[string]func(*CheckpointRemovalReceipt){
		"removed":           func(*CheckpointRemovalReceipt) {},
		"nothing to remove": func(receipt *CheckpointRemovalReceipt) { receipt.Status = CheckpointNothingToRemove },
		"failed": func(receipt *CheckpointRemovalReceipt) {
			receipt.Status, receipt.ReasonCode = CheckpointUnavailable, CheckpointRemovalFailed
		},
		"result unavailable": func(receipt *CheckpointRemovalReceipt) {
			receipt.Status, receipt.ReasonCode = CheckpointUnavailable, CheckpointRemovalResultUnavailable
		},
		"not launched": func(receipt *CheckpointRemovalReceipt) {
			receipt.Status, receipt.ReasonCode = CheckpointUnavailable, CheckpointRemovalNotLaunched
			receipt.TerminationSHA256, receipt.WorkerWaitSHA256, receipt.UnlaunchedSHA256 = "", "", repeatHex('3', 64)
		},
		"failed after previous boot": func(receipt *CheckpointRemovalReceipt) {
			receipt.Status, receipt.ReasonCode = CheckpointUnavailable, CheckpointRemovalFailed
			removalPreviousBoot(receipt)
		},
	}
	for name, mutate := range valid {
		receipt := removalReceipt(attempt)
		mutate(&receipt)
		if err := ValidateCheckpointRemovalReceipt(receipt, attempt); err != nil {
			t.Fatalf("%s receipt rejected: %v", name, err)
		}
	}
}

// removalPreviousBoot records the legacy closure encoding: termination only.
func removalPreviousBoot(receipt *CheckpointRemovalReceipt) {
	receipt.LifetimeClosure, receipt.WorkerWaitSHA256 = task.LifetimeClosureLegacyPreviousBoot, ""
}

func TestCheckpointRemovalSuccessRequiresNativeClosureEncoding(t *testing.T) {
	attempt := removalReference(2, repeatHex('b', 32))
	for _, status := range []string{CheckpointRemoved, CheckpointNothingToRemove} {
		receipt := removalReceipt(attempt)
		receipt.Status = status
		removalPreviousBoot(&receipt)
		if _, err := ValidateClosureEvidence(receipt.Closure()); err != nil {
			t.Fatalf("shared legacy closure validation changed: %v", err)
		}
		if err := ValidateCheckpointRemovalReceipt(receipt, attempt); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s after previous boot error=%v, want identity mismatch", status, err)
		}
		receipt = removalReceipt(attempt)
		receipt.Status = status
		receipt.LifetimeClosure, receipt.TerminationSHA256 = task.LifetimeClosureUnregisteredRecovered, ""
		if err := ValidateCheckpointRemovalReceipt(receipt, attempt); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s after unregistered recovery error=%v, want identity mismatch", status, err)
		}
	}
}

func TestCheckpointRemovalReceiptRefusesUnprovenOutcomes(t *testing.T) {
	attempt := removalReference(2, repeatHex('b', 32))
	unlaunched := func(receipt *CheckpointRemovalReceipt) {
		receipt.TerminationSHA256, receipt.WorkerWaitSHA256, receipt.UnlaunchedSHA256 = "", "", repeatHex('3', 64)
	}
	cases := map[string]func(*CheckpointRemovalReceipt){
		"schema":              func(receipt *CheckpointRemovalReceipt) { receipt.SchemaVersion = 0 },
		"other attempt":       func(receipt *CheckpointRemovalReceipt) { receipt.Attempt.Sequence = 1 },
		"other request":       func(receipt *CheckpointRemovalReceipt) { receipt.Attempt.RequestSHA256 = repeatHex('9', 64) },
		"removed with reason": func(receipt *CheckpointRemovalReceipt) { receipt.ReasonCode = CheckpointRemovalFailed },
		"removed without run": unlaunched,
		"nothing without run": func(receipt *CheckpointRemovalReceipt) {
			unlaunched(receipt)
			receipt.Status = CheckpointNothingToRemove
		},
		"unavailable no reason": func(receipt *CheckpointRemovalReceipt) { receipt.Status = CheckpointUnavailable },
		"creation reason": func(receipt *CheckpointRemovalReceipt) {
			receipt.Status, receipt.ReasonCode = CheckpointUnavailable, CheckpointFailed
		},
		"launched claims not launched": func(receipt *CheckpointRemovalReceipt) {
			receipt.Status, receipt.ReasonCode = CheckpointUnavailable, CheckpointRemovalNotLaunched
		},
		"replaced common dir is not a status": func(receipt *CheckpointRemovalReceipt) { receipt.Status = "replaced" },
		"open lifetime":                       func(receipt *CheckpointRemovalReceipt) { receipt.LifetimeClosure = task.LifetimeClosureOpen },
	}
	for name, mutate := range cases {
		receipt := removalReceipt(attempt)
		mutate(&receipt)
		if err := ValidateCheckpointRemovalReceipt(receipt, attempt); err == nil {
			t.Fatalf("%s receipt accepted", name)
		}
	}
	malformed := attempt
	malformed.Sequence = 0
	if err := ValidateCheckpointRemovalReceipt(removalReceipt(malformed), malformed); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("receipt for malformed attempt error=%v", err)
	}
}
