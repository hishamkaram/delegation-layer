package execution

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestNativeTimeoutClassificationRequiresProvenCompletion(t *testing.T) {
	proven := nativeRecordedError(nativeCommandResult{ExitCode: -1, Expired: true})
	if !NativeCommandTimedOut(proven) || !errors.Is(proven, context.DeadlineExceeded) {
		t.Fatalf("proven timeout lost: %v", proven)
	}
	for _, err := range []error{context.DeadlineExceeded, errors.Join(proven, io.EOF), errors.Join(proven, context.Canceled), nativeRecordedError(nativeCommandResult{ExitCode: -1, Expired: true, Fault: "bounded helper fault"})} {
		if NativeCommandTimedOut(err) {
			t.Fatalf("uncertain completion became timeout: %v", err)
		}
	}
}
