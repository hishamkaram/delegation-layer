package antigravity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRuntimeFingerprintDetectsContentChange(t *testing.T) {
	path := filepath.Join(sourceTestRoot(t), "agy")
	data := []byte("in-memory test executable bytes; never launched")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := hashRuntimeExecutable(path)
	if err != nil || before != task.ComputeSHA256(data) {
		t.Fatalf("runtime digest %s: %v", before, err)
	}
	if err = os.WriteFile(path, []byte("different runtime bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := hashRuntimeExecutable(path)
	if err != nil || after == before {
		t.Fatalf("runtime change not detected: %s, %v", after, err)
	}
}

func TestRuntimeFingerprintRejectsNonExecutableAndAlias(t *testing.T) {
	root := sourceTestRoot(t)
	path := filepath.Join(root, "agy")
	writeSourceTestFile(t, path, []byte("not executable"))
	if _, err := hashRuntimeExecutable(path); err == nil {
		t.Fatal("non-executable accepted")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{alias, root, "relative"} {
		if _, err := hashRuntimeExecutable(invalid); err == nil {
			t.Errorf("invalid executable accepted: %s", invalid)
		}
	}
}
