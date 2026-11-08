package tasklifecycle

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRemoteDuplicateCancellationPreservesJoinedFaults(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		duplicate bool
	}{
		{"absent", nil, false},
		{"duplicate", task.ErrStopAlreadyRequested, true},
		{"wrapped", fmt.Errorf("request: %w", task.ErrStopAlreadyRequested), true},
		{"only duplicates", errors.Join(task.ErrStopAlreadyRequested, task.ErrStopAlreadyRequested), true},
		{"read fault", errors.Join(task.ErrStopAlreadyRequested, io.ErrUnexpectedEOF), false},
		{"cleanup fault", errors.Join(task.ErrStopAlreadyRequested, os.ErrPermission), false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := duplicateCancellationOnly(test.err); got != test.duplicate {
				t.Fatalf("duplicate=%t, want%t: %v", got, test.duplicate, test.err)
			}
		})
	}
}
