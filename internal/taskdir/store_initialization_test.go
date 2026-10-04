package taskdir

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRootInitializationAdoptsConcurrentWinner(t *testing.T) {
	store := testStore(t)
	rootPath := filepath.Join(store.Root, "root.json")
	before, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	winnerID := store.RootID
	store.RootID = ""
	// Deterministically model another initializer winning after our existence
	// check but before the create-once link. No scheduling or sleeps are needed.
	if initErr := store.createNewRoot(store.Root); initErr != nil {
		t.Fatal(initErr)
	}
	if store.RootID != winnerID {
		t.Fatalf("root ID = %q, want existing winner %q", store.RootID, winnerID)
	}
	after, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("initialization replaced the winning root record")
	}
	stages, err := filepath.Glob(filepath.Join(store.Root, "stage.*.tmp"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("losing initializer retained stages %v: %v", stages, err)
	}
}

func TestRootInitializationRejectsInvalidWinner(t *testing.T) {
	for _, kind := range []string{"malformed", "unsafe-mode", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			store := testStore(t)
			rootPath := filepath.Join(store.Root, "root.json")
			makeInvalidRootWinner(t, rootPath, kind)
			store.RootID = ""
			if err := store.createNewRoot(store.Root); err == nil || store.RootID != "" {
				t.Fatalf("invalid winner accepted: root=%q error=%v", store.RootID, err)
			}
		})
	}
}

func makeInvalidRootWinner(t *testing.T, rootPath, kind string) {
	t.Helper()
	switch kind {
	case "malformed":
		if err := os.WriteFile(rootPath, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "unsafe-mode":
		if err := os.Chmod(rootPath, 0o644); err != nil {
			t.Fatal(err)
		}
	case "symlink":
		if err := os.Rename(rootPath, filepath.Join(filepath.Dir(rootPath), "other.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("other.json", rootPath); err != nil {
			t.Fatal(err)
		}
	}
}
