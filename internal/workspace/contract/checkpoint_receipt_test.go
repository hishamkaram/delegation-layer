package contract

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func launchedClosure() ClosureEvidence {
	return ClosureEvidence{TerminationSHA256: repeatHex('1', 64), WorkerWaitSHA256: repeatHex('2', 64)}
}

func TestValidateClosureEvidenceAcceptsOnlyCanonicalClosedShapes(t *testing.T) {
	digest := repeatHex('3', 64)
	cases := []struct {
		name    string
		closure ClosureEvidence
		ran     bool
		valid   bool
	}{
		{name: "native termination and worker wait", closure: launchedClosure(), ran: true, valid: true},
		{name: "unlaunched", closure: ClosureEvidence{UnlaunchedSHA256: digest}, valid: true},
		{name: "unregistered recovered", closure: ClosureEvidence{Kind: task.LifetimeClosureUnregisteredRecovered, WorkerWaitSHA256: digest}, valid: true},
		{name: "legacy previous boot", closure: ClosureEvidence{Kind: task.LifetimeClosureLegacyPreviousBoot, TerminationSHA256: digest}, ran: true, valid: true},
		{name: "missing worker wait", closure: ClosureEvidence{TerminationSHA256: digest}},
		{name: "unlaunched with termination", closure: ClosureEvidence{UnlaunchedSHA256: digest, TerminationSHA256: digest}},
		{name: "unregistered with termination", closure: ClosureEvidence{Kind: task.LifetimeClosureUnregisteredRecovered, WorkerWaitSHA256: digest, TerminationSHA256: digest}},
		{name: "legacy with worker wait", closure: ClosureEvidence{Kind: task.LifetimeClosureLegacyPreviousBoot, TerminationSHA256: digest, WorkerWaitSHA256: digest}},
		{name: "open", closure: ClosureEvidence{Kind: task.LifetimeClosureOpen, TerminationSHA256: digest, WorkerWaitSHA256: digest}},
		{name: "registered native", closure: ClosureEvidence{Kind: task.LifetimeClosureRegisteredNative, TerminationSHA256: digest, WorkerWaitSHA256: digest}},
		{name: "registered recovered", closure: ClosureEvidence{Kind: task.LifetimeClosureRegisteredRecovered, TerminationSHA256: digest, WorkerWaitSHA256: digest}},
		{name: "unknown", closure: ClosureEvidence{Kind: "closed", TerminationSHA256: digest, WorkerWaitSHA256: digest}},
		{name: "empty", closure: ClosureEvidence{}},
	}
	for _, testCase := range cases {
		ran, err := ValidateClosureEvidence(testCase.closure)
		if (err == nil) != testCase.valid || (testCase.valid && ran != testCase.ran) {
			t.Fatalf("%s: ran=%v err=%v, want ran=%v valid=%v", testCase.name, ran, err, testCase.ran, testCase.valid)
		}
	}
}

func TestCheckpointSuccessClosureRequiresDefaultNativeEncoding(t *testing.T) {
	native := launchedClosure()
	if err := validateCheckpointSuccessClosure(native); err != nil {
		t.Fatalf("native success closure rejected: %v", err)
	}
	digest := repeatHex('3', 64)
	refused := map[string]ClosureEvidence{
		"recovery kind with native digests": {Kind: task.LifetimeClosureLegacyPreviousBoot, TerminationSHA256: native.TerminationSHA256, WorkerWaitSHA256: native.WorkerWaitSHA256},
		"legacy termination only":           {Kind: task.LifetimeClosureLegacyPreviousBoot, TerminationSHA256: digest},
		"unregistered worker wait only":     {Kind: task.LifetimeClosureUnregisteredRecovered, WorkerWaitSHA256: digest},
		"termination without worker wait":   {TerminationSHA256: digest},
		"worker wait without termination":   {WorkerWaitSHA256: digest},
		"native digests and unlaunched":     {TerminationSHA256: digest, WorkerWaitSHA256: digest, UnlaunchedSHA256: digest},
		"unlaunched":                        {UnlaunchedSHA256: digest},
	}
	for name, closure := range refused {
		if err := validateCheckpointSuccessClosure(closure); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s: error=%v, want identity mismatch", name, err)
		}
	}
}

func checkpointReceipt(t *testing.T, request OperationRequest) CheckpointReceipt {
	t.Helper()
	candidate := checkpointCandidate(t, request, ordinaryTarget())
	closure := launchedClosure()
	return CheckpointReceipt{
		SchemaVersion: task.SchemaVersion, OperationID: request.OperationID, RequestSHA256: candidateRequestSHA256,
		Status: CheckpointCheckpointed, Candidate: &candidate, TerminationSHA256: closure.TerminationSHA256, WorkerWaitSHA256: closure.WorkerWaitSHA256,
	}
}

func unavailable(reason string) func(*CheckpointReceipt) {
	return func(receipt *CheckpointReceipt) {
		receipt.Status, receipt.ReasonCode, receipt.Candidate = CheckpointUnavailable, reason, nil
	}
}

func notLaunched(receipt *CheckpointReceipt) {
	unavailable(CheckpointNotLaunched)(receipt)
	receipt.TerminationSHA256, receipt.WorkerWaitSHA256, receipt.UnlaunchedSHA256 = "", "", repeatHex('3', 64)
}

// previousBoot records the legacy closure encoding: termination only.
func previousBoot(receipt *CheckpointReceipt) {
	receipt.LifetimeClosure, receipt.WorkerWaitSHA256 = task.LifetimeClosureLegacyPreviousBoot, ""
}

func TestCheckpointReceiptRecordsHistoricalDisposition(t *testing.T) {
	valid := map[string]func(*CheckpointReceipt){
		"checkpointed": func(*CheckpointReceipt) {},
		"failed after previous boot": func(receipt *CheckpointReceipt) {
			unavailable(CheckpointFailed)(receipt)
			previousBoot(receipt)
		},
		"result unavailable after previous boot": func(receipt *CheckpointReceipt) {
			unavailable(CheckpointResultUnavailable)(receipt)
			previousBoot(receipt)
		},
		"failed":             unavailable(CheckpointFailed),
		"result unavailable": unavailable(CheckpointResultUnavailable),
		"not launched":       notLaunched,
		"unregistered intent": func(receipt *CheckpointReceipt) {
			unavailable(CheckpointNotLaunched)(receipt)
			receipt.LifetimeClosure, receipt.TerminationSHA256 = task.LifetimeClosureUnregisteredRecovered, ""
		},
	}
	for name, mutate := range valid {
		request := checkpointRequest(ordinarySelection())
		receipt := checkpointReceipt(t, request)
		mutate(&receipt)
		if err := ValidateCheckpointReceipt(receipt, request, candidateRequestSHA256); err != nil {
			t.Fatalf("%s receipt rejected: %v", name, err)
		}
	}
}

func TestCheckpointReceiptSuccessRequiresNativeClosureEncoding(t *testing.T) {
	request := checkpointRequest(ordinarySelection())
	receipt := checkpointReceipt(t, request)
	previousBoot(&receipt)
	if _, err := ValidateClosureEvidence(receipt.Closure()); err != nil {
		t.Fatalf("shared legacy closure validation changed: %v", err)
	}
	if err := ValidateCheckpointReceipt(receipt, request, candidateRequestSHA256); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("checkpointed after previous boot error=%v, want identity mismatch", err)
	}
	unregistered := checkpointReceipt(t, request)
	unregistered.LifetimeClosure, unregistered.TerminationSHA256 = task.LifetimeClosureUnregisteredRecovered, ""
	if err := ValidateCheckpointReceipt(unregistered, request, candidateRequestSHA256); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("checkpointed after unregistered recovery error=%v, want identity mismatch", err)
	}
}

func TestCheckpointReceiptRefusesMalformedDisposition(t *testing.T) {
	cases := map[string]func(*CheckpointReceipt, *OperationRequest){
		"schema":                     func(receipt *CheckpointReceipt, _ *OperationRequest) { receipt.SchemaVersion = 0 },
		"operation":                  func(receipt *CheckpointReceipt, _ *OperationRequest) { receipt.OperationID = repeatHex('9', 32) },
		"request digest":             func(receipt *CheckpointReceipt, _ *OperationRequest) { receipt.RequestSHA256 = repeatHex('9', 64) },
		"removal request":            func(_ *CheckpointReceipt, request *OperationRequest) { request.Kind = OperationCheckpointRemove },
		"checkpointed without one":   func(receipt *CheckpointReceipt, _ *OperationRequest) { receipt.Candidate = nil },
		"checkpointed with reason":   func(receipt *CheckpointReceipt, _ *OperationRequest) { receipt.ReasonCode = CheckpointFailed },
		"checkpointed bad candidate": func(receipt *CheckpointReceipt, _ *OperationRequest) { receipt.Candidate.TreeOID = repeatHex('f', 40) },
		"checkpointed not launched": func(receipt *CheckpointReceipt, _ *OperationRequest) {
			receipt.TerminationSHA256, receipt.WorkerWaitSHA256, receipt.UnlaunchedSHA256 = "", "", repeatHex('3', 64)
		},
		"unavailable with candidate": func(receipt *CheckpointReceipt, _ *OperationRequest) {
			receipt.Status, receipt.ReasonCode = CheckpointUnavailable, CheckpointFailed
		},
		"unavailable without reason":   func(receipt *CheckpointReceipt, _ *OperationRequest) { unavailable("")(receipt) },
		"unavailable unknown reason":   func(receipt *CheckpointReceipt, _ *OperationRequest) { unavailable("capture_failed")(receipt) },
		"launched claims not launched": func(receipt *CheckpointReceipt, _ *OperationRequest) { unavailable(CheckpointNotLaunched)(receipt) },
		"unlaunched claims failure": func(receipt *CheckpointReceipt, _ *OperationRequest) {
			notLaunched(receipt)
			receipt.ReasonCode = CheckpointFailed
		},
		"open lifetime": func(receipt *CheckpointReceipt, _ *OperationRequest) {
			receipt.LifetimeClosure = task.LifetimeClosureOpen
		},
		"unknown status": func(receipt *CheckpointReceipt, _ *OperationRequest) { receipt.Status = "captured" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := checkpointRequest(ordinarySelection())
			receipt := checkpointReceipt(t, request)
			mutate(&receipt, &request)
			if err := ValidateCheckpointReceipt(receipt, request, candidateRequestSHA256); err == nil {
				t.Fatal("malformed receipt accepted")
			}
		})
	}
}
