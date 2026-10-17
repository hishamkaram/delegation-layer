package supervisor

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func closeAppTestTask(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir) {
	t.Helper()
	if err := td.Close(); err != nil {
		t.Error(err)
	}
	if err := store.Close(); err != nil {
		t.Error(err)
	}
}
