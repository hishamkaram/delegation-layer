//go:build darwin || linux

package execution

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestObservedWaitErrorAcceptsObservedExitState(t *testing.T) {
	// This in-memory fixture verifies classification by observed-state presence.
	// It does not simulate signal termination; ProcessState has no public
	// constructor for a signaled state.
	waitErr := &exec.ExitError{ProcessState: &os.ProcessState{}}

	if got := observedWaitError(waitErr); got != nil {
		t.Fatalf("observed exit state returned an error: %v", got)
	}
}

func TestObservedWaitErrorPreservesUnobservedAndJoinedErrors(t *testing.T) {
	infrastructure := errors.New("capture infrastructure failed")
	observedExit := &exec.ExitError{ProcessState: &os.ProcessState{}}
	nilStateExit := &exec.ExitError{}
	joined := errors.Join(observedExit, infrastructure)
	wrapped := fmt.Errorf("wait: %w", infrastructure)

	tests := []struct {
		name   string
		err    error
		want   error
		causes []error
	}{
		{name: "nil", err: nil, want: nil},
		{name: "nil process state", err: nilStateExit, want: nilStateExit},
		{name: "joined infrastructure fault", err: joined, want: joined, causes: []error{observedExit, infrastructure}},
		{name: "ordinary infrastructure fault", err: infrastructure, want: infrastructure, causes: []error{infrastructure}},
		{name: "wrapped infrastructure fault", err: wrapped, want: wrapped, causes: []error{infrastructure}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := observedWaitError(tt.err)
			if !errors.Is(got, tt.want) {
				t.Fatalf("returned error %v, want %v", got, tt.want)
			}
			for _, cause := range tt.causes {
				if !errors.Is(got, cause) {
					t.Fatalf("returned error %v lost cause %v", got, cause)
				}
			}
		})
	}
}
