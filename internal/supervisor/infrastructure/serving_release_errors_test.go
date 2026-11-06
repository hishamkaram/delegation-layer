package infrastructure

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestServingReleasePutBoundaryPreservesIndependentCleanupFailures(t *testing.T) {
	putFailure := &servingReleasePutError{cause: syscall.EIO}
	otherPut := &servingReleasePutError{cause: syscall.EIO}
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"actual Put", putFailure, true},
		{"wrapped actual Put", fmt.Errorf("transaction: %w", putFailure), true},
		{"joined actual Put", errors.Join(putFailure), true},
		{"same errno from cleanup", errors.Join(putFailure, syscall.EIO), false},
		{"different cleanup fault", errors.Join(putFailure, syscall.EBADF), false},
		{"wrapped joined same errno", fmt.Errorf("transaction: %w", errors.Join(putFailure, syscall.EIO)), false},
		{"different Put boundary", otherPut, false},
		{"different Put boundary wrapping original", &servingReleasePutError{cause: putFailure}, false},
		{"untagged same errno", syscall.EIO, false},
		{"absent error", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := onlyServingReleasePutFailure(test.err, putFailure); got != test.want {
				t.Fatalf("release permission=%t want=%t for %v", got, test.want, test.err)
			}
		})
	}
	if !errors.Is(putFailure, syscall.EIO) {
		t.Fatal("Put provenance boundary hid its original cause")
	}
}
