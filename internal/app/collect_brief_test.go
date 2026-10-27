package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TestCollectBriefIdentityFaultIsOperationalFailureOnTheCLI changes the brief
// of a pending task between public collect calls. The concrete identity fault
// must exit 1 with its error and no pending state; neither the earlier
// pending exit 3 nor a cached result may be replayed, and restoring the brief
// returns the task to exit 3.
func TestCollectBriefIdentityFaultIsOperationalFailureOnTheCLI(t *testing.T) {
	for name, changed := range map[string]string{"truncated": "brie", "same-length": "briex"} {
		t.Run(name, func(t *testing.T) {
			store, td, req := newAppTestTask(t, false)
			defer closeAppTestTask(t, store, td)
			briefPath := filepath.Join(td.Dir, "brief.md")

			code, response, _ := collectWatchCLI(t, store.Root, req.TaskID, "0s")
			requirePendingPublication(t, "intact brief", code, response)

			writeAppTestFile(t, briefPath, []byte(changed))
			code, response, _ = collectWatchCLI(t, store.Root, req.TaskID, "0s")
			if code != 1 || !strings.Contains(response.Error, task.ErrIdentityMismatch.Error()) || response.Pending != nil || response.Outcome != nil {
				t.Fatalf("changed brief = exit %d %+v, want operational failure %q", code, response, task.ErrIdentityMismatch)
			}

			if err := os.WriteFile(briefPath, []byte("brief"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, response, _ = collectWatchCLI(t, store.Root, req.TaskID, "0s")
			requirePendingPublication(t, "restored brief", code, response)
		})
	}
}
