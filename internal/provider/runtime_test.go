package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestFingerprintExecutableRejectsAliasesAndSpecialFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "provider")
	data := []byte("finite fixture; never executed")
	if err = os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := FingerprintExecutable(path)
	if err != nil || digest != task.ComputeSHA256(data) {
		t.Fatalf("digest=%s err=%v", digest, err)
	}
	alias := filepath.Join(root, "alias")
	if err = os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err = unix.Mkfifo(fifo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{root, alias, fifo, "relative"} {
		if _, err = FingerprintExecutable(invalid); err == nil {
			t.Errorf("accepted %s", invalid)
		}
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = FingerprintExecutable(path); err == nil {
		t.Fatal("accepted non-executable")
	}
}

func TestFingerprintOpenedRejectsChangedPathIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(path, []byte("original"), 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("replaced"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = fingerprintOpened(path, file, before); err == nil {
		t.Fatal("accepted replaced executable path")
	}
}
