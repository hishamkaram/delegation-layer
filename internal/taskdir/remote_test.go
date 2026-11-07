package taskdir

import (
	"context"
	"os"
	"testing"
)

func TestRemoteReferenceDoesNotCreateTask(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	journal, err := store.OpenRemoteReferenceContext(context.Background(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	exists, err := store.TaskEntryExists(id)
	if err != nil || exists {
		t.Fatalf("ordinary task created: %t %v", exists, err)
	}
}
