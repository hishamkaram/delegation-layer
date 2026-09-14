package antigravity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestPolicySourcePresenceAndRegularFile(t *testing.T) {
	root := sourceTestRoot(t)
	path := filepath.Join(root, "settings.json")
	absent, err := readPolicySource(path)
	if err != nil || absent.Present || absent.Data != nil {
		t.Fatalf("absent source = %+v, %v", absent, err)
	}
	writeSourceTestFile(t, path, []byte(`{"model":"example"}`))
	present, err := readPolicySource(path)
	if err != nil || !present.Present || string(present.Data) != `{"model":"example"}` || present.Path != path {
		t.Fatalf("present source = %+v, %v", present, err)
	}
}

func TestPolicySourceRejectsAliasesAndNonRegularFiles(t *testing.T) {
	root := sourceTestRoot(t)
	file := filepath.Join(root, "settings.json")
	writeSourceTestFile(t, file, []byte(`{}`))
	link := filepath.Join(root, "alias.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	directoryLink := filepath.Join(root, "alias-dir")
	if err := os.Symlink(root, directoryLink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, link, fifo, filepath.Join(directoryLink, "absent.json"), "relative.json"} {
		if _, err := readPolicySource(path); !errors.Is(err, ErrUnsupportedProfile) {
			t.Errorf("accepted unsafe source %s: %v", path, err)
		}
	}
}

func TestPolicySourceRejectsOversizeAndReplacedInode(t *testing.T) {
	path := filepath.Join(sourceTestRoot(t), "settings.json")
	writeSourceTestFile(t, path, make([]byte, task.MaxControlRecordSize+1))
	if _, err := readPolicySource(path); !errors.Is(err, task.ErrControlRecordTooBig) || !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("oversized source accepted: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".replacement"
	writeSourceTestFile(t, replacement, []byte(`{}`))
	if err = os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err = readStablePolicyFile(path, before); err == nil {
		t.Fatal("replacement inode accepted")
	}
}

func TestPolicySourceDetectsChangeAfterRead(t *testing.T) {
	path := filepath.Join(sourceTestRoot(t), "settings.json")
	writeSourceTestFile(t, path, []byte(`{}`))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := f.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	writeSourceTestFile(t, path, []byte(`{"changed":true}`))
	if err = checkPolicyFileAfterRead(path, f, before, 2); err == nil {
		t.Fatal("mutation after read accepted")
	}
}

func sourceTestRoot(t *testing.T) string {
	t.Helper()
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeSourceTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
