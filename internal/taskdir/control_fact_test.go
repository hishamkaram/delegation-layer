package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestControlLockFailureNamesFactWithoutChangingAbsence(t *testing.T) {
	for _, unsafeMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "unsafe mode"}[unsafeMode], func(t *testing.T) {
			store := testStore(t)
			id := strings.Repeat("b", 32)
			dir := filepath.Join(store.Root, "inspections", id)
			must(t, os.MkdirAll(dir, 0o700))
			if unsafeMode {
				must(t, os.WriteFile(filepath.Join(dir, ".operation.lock"), nil, 0o600))
				must(t, os.Chmod(filepath.Join(dir, ".operation.lock"), 0o644))
			}
			control, err := store.OpenInspectionContext(t.Context(), id, false)
			if control != nil {
				must(t, control.Close())
				t.Fatal("invalid existing lock was opened")
			}
			var fact *task.ControlFactError
			if !errors.As(err, &fact) || fact.ResourceKind != "inspection" || fact.ResourceID != id || fact.Name != ".operation.lock" {
				t.Fatalf("missing lock fact: %v", err)
			}
			if task.RecordAbsent(err) == unsafeMode {
				t.Fatalf("lock fault changed absence authority: %v", err)
			}
			if unsafeMode && (!errors.Is(err, task.ErrEvidenceFault) || fact.ReasonCode != "evidence_unreadable") {
				t.Fatalf("private-mode violation was not retained: %v", err)
			}
		})
	}
}

func TestControlFactJoinedMissingAndFaultIsUnreadable(t *testing.T) {
	store := testStore(t)
	cause := errors.Join(os.ErrNotExist, os.ErrPermission)
	err := store.recordFactError(filepath.Join(store.Root, "root.json"), cause)
	var fact *task.ControlFactError
	if !errors.As(err, &fact) || fact.ReasonCode != "evidence_unreadable" || task.RecordAbsent(err) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("mixed missing/storage fault was misclassified: %v", err)
	}
}
