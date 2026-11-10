package tasklifecycle

import (
	"bytes"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadBriefInputCopiesAndRejectsConflictingOrInvalidSources(t *testing.T) {
	source := []byte("exact request\n")
	got, err := ReadBriefInput("", source)
	if err != nil || !bytes.Equal(got, source) {
		t.Fatalf("text input = %q, %v", got, err)
	}
	source[0] = 'X'
	if string(got) != "exact request\n" {
		t.Fatal("request retained mutable caller content")
	}
	if _, err := ReadBriefInput("/unused", source); err == nil {
		t.Fatal("conflicting file/text sources accepted")
	}
	if _, err := ReadBriefInput("", []byte{}); !errors.Is(err, config.ErrBriefEmpty) {
		t.Fatalf("empty text = %v", err)
	}
	if _, err := ReadBriefInput("", make([]byte, task.MaxBriefSize+1)); !errors.Is(err, config.ErrBriefTooLarge) {
		t.Fatalf("oversize text = %v", err)
	}
}

func TestContinuationCallerIdentityValidatedBeforeOpeningState(t *testing.T) {
	result := ContinueContext(t.Context(), DispatchRequest{}, ContinuationOptions{SuccessorTaskID: "invalid"})
	if result.Phase != ContinueValidation || result.Failure == nil || result.Root != "" {
		t.Fatalf("invalid successor reached state resolution: %+v", result)
	}
	const requested = "0123456789abcdef0123456789abcdef"
	if id, err := continuationSuccessorID(requested); err != nil || id != requested {
		t.Fatalf("caller ID changed: %q, %v", id, err)
	}
	id, err := continuationSuccessorID("")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.ValidateTaskID(id); err != nil {
		t.Fatalf("default generator did not produce a valid ID: %v", err)
	}
}
