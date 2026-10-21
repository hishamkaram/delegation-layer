package journal

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TestCaptureClosureKeepsHistoricalErrorOrder pins the capture-specific
// closure decisions: a never-launched closure checks its unavailable status,
// reason, candidate and artifacts before its digests, and every other closure
// shape keeps its historical classification.
func TestCaptureClosureKeepsHistoricalErrorOrder(t *testing.T) {
	digest, malformed := testSHA('a'), "not-a-digest"
	notLaunched := func(receipt *CaptureReceipt) {
		receipt.Status, receipt.ReasonCode = "unavailable", "capture_not_launched"
	}
	captured := func(receipt *CaptureReceipt) { receipt.Status = "captured" }
	cases := []struct {
		name    string
		closure func(*CaptureReceipt)
		status  func(*CaptureReceipt)
		want    string
	}{
		{name: "native", closure: nativeCaptureClosure(digest, digest), status: captured, want: "valid"},
		{name: "native missing worker wait", closure: nativeCaptureClosure(digest, ""), status: captured, want: "other"},
		{name: "unlaunched", closure: unlaunchedCaptureClosure(digest), status: notLaunched, want: "valid"},
		{name: "unlaunched captured", closure: unlaunchedCaptureClosure(digest), status: captured, want: "identity"},
		{name: "unlaunched malformed and captured", closure: unlaunchedCaptureClosure(malformed), status: captured, want: "identity"},
		{name: "unlaunched malformed", closure: unlaunchedCaptureClosure(malformed), status: notLaunched, want: "other"},
		{name: "unlaunched with termination", closure: func(receipt *CaptureReceipt) {
			receipt.UnlaunchedSHA256, receipt.TerminationSHA256 = digest, digest
		}, status: notLaunched, want: "identity"},
		{name: "unlaunched with artifact", closure: func(receipt *CaptureReceipt) {
			receipt.UnlaunchedSHA256, receipt.Artifacts = digest, []Artifact{{Name: "diff.patch", Omitted: "limit"}}
		}, status: notLaunched, want: "identity"},
		{name: "unregistered", closure: unregisteredCaptureClosure(digest), status: notLaunched, want: "valid"},
		{name: "unregistered malformed and captured", closure: unregisteredCaptureClosure(malformed), status: captured, want: "identity"},
		{name: "unregistered malformed", closure: unregisteredCaptureClosure(malformed), status: notLaunched, want: "other"},
		{name: "unregistered with termination", closure: func(receipt *CaptureReceipt) {
			unregisteredCaptureClosure(digest)(receipt)
			receipt.TerminationSHA256 = digest
		}, status: notLaunched, want: "identity"},
		{name: "previous boot captured", closure: previousBootCaptureClosure(digest), status: captured, want: "valid"},
		{name: "previous boot malformed", closure: previousBootCaptureClosure(malformed), status: captured, want: "other"},
		{name: "previous boot with worker wait", closure: func(receipt *CaptureReceipt) {
			previousBootCaptureClosure(digest)(receipt)
			receipt.WorkerWaitSHA256 = digest
		}, status: captured, want: "identity"},
		{name: "open", closure: kindCaptureClosure(task.LifetimeClosureOpen), status: notLaunched, want: "enum"},
		{name: "registered native", closure: kindCaptureClosure(task.LifetimeClosureRegisteredNative), status: captured, want: "enum"},
		{name: "registered recovered", closure: kindCaptureClosure(task.LifetimeClosureRegisteredRecovered), status: captured, want: "enum"},
		{name: "unknown", closure: kindCaptureClosure("closed"), status: notLaunched, want: "enum"},
	}
	for _, testCase := range cases {
		receipt := CaptureReceipt{SchemaVersion: task.SchemaVersion, WorkspaceID: testID('a'), OperationID: testID('b'), Subject: subjectFor('c')}
		testCase.status(&receipt)
		testCase.closure(&receipt)
		if got := closureErrorClass(validateCaptureClosure(receipt)); got != testCase.want {
			t.Errorf("%s: closure class=%s, want %s", testCase.name, got, testCase.want)
		}
	}
}

func nativeCaptureClosure(termination, workerWait string) func(*CaptureReceipt) {
	return func(receipt *CaptureReceipt) {
		receipt.TerminationSHA256, receipt.WorkerWaitSHA256 = termination, workerWait
	}
}

func unlaunchedCaptureClosure(unlaunched string) func(*CaptureReceipt) {
	return func(receipt *CaptureReceipt) { receipt.UnlaunchedSHA256 = unlaunched }
}

func unregisteredCaptureClosure(workerWait string) func(*CaptureReceipt) {
	return func(receipt *CaptureReceipt) {
		receipt.LifetimeClosure, receipt.WorkerWaitSHA256 = task.LifetimeClosureUnregisteredRecovered, workerWait
	}
}

func previousBootCaptureClosure(termination string) func(*CaptureReceipt) {
	return func(receipt *CaptureReceipt) {
		receipt.LifetimeClosure, receipt.TerminationSHA256 = task.LifetimeClosureLegacyPreviousBoot, termination
	}
}

func kindCaptureClosure(kind task.LifetimeClosureKind) func(*CaptureReceipt) {
	return func(receipt *CaptureReceipt) {
		receipt.LifetimeClosure, receipt.TerminationSHA256, receipt.WorkerWaitSHA256 = kind, testSHA('d'), testSHA('e')
	}
}

func closureErrorClass(err error) string {
	switch {
	case err == nil:
		return "valid"
	case errors.Is(err, task.ErrInvalidEnum):
		return "enum"
	case errors.Is(err, task.ErrIdentityMismatch):
		return "identity"
	default:
		return "other"
	}
}
