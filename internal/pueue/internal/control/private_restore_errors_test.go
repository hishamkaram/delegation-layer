package control

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestPrivateRestoreFaultClassificationPreservesOperationalCauses(t *testing.T) {
	readFault, closeFault := errors.New("read failed"), errors.New("close failed")
	for _, operational := range []error{context.Canceled, context.DeadlineExceeded, &os.PathError{Op: "read", Err: readFault}, &privateRestoreReadError{cause: readFault}} {
		cause := errors.Join(operational, closeFault)
		for _, kind := range []error{ErrPrivateRestoreLayout, ErrPrivateRestoreCorrupt, ErrPrivateRestorePartial} {
			got := privateRestoreFault(kind, cause)
			if errors.Is(got, kind) || !errors.Is(got, operational) || !errors.Is(got, closeFault) {
				t.Fatalf("operational cause lost or relabeled: %v", got)
			}
		}
	}
}
