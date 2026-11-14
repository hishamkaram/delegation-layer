package gatewayservice

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsPendingRequiresWholeErrorQualification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"absent", nil, false},
		{"pending", ErrPending, true},
		{"wrapped pending", fmt.Errorf("stop: %w", ErrPending), true},
		{"pending deadline", errors.Join(ErrPending, context.DeadlineExceeded), true},
		{"pending cancellation", errors.Join(ErrPending, context.Canceled), true},
		{"nested pending deadlines", fmt.Errorf("stop: %w", errors.Join(ErrPending, fmt.Errorf("observe: %w", context.DeadlineExceeded))), true},
		{"deadline only", context.DeadlineExceeded, false},
		{"pending foreign service", errors.Join(ErrPending, ErrForeignService), false},
		{"pending manager", errors.Join(ErrPending, ErrManager), false},
		{"pending configuration", errors.Join(ErrPending, ErrConfiguration), false},
		{"pending unknown observation", errors.Join(ErrPending, errors.New("observation failed")), false},
		{"nested concrete fault", errors.Join(fmt.Errorf("stop: %w", errors.Join(ErrPending, context.Canceled)), errors.New("close failed")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPending(tc.err); got != tc.want {
				t.Fatalf("IsPending(%v)=%t, want %t", tc.err, got, tc.want)
			}
		})
	}
}
