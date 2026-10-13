package app

import (
	"os"
	"path/filepath"
	"testing"
)

func canonicalAppTestTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "dla-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(directory); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	return canonicalAppTestPath(t, directory)
}

func canonicalAppTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func canonicalAppTestExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return canonicalAppTestPath(t, executable)
}

func TestCanonicalAppTestPathResolvesSymlinkedTempRoot(t *testing.T) {
	base := canonicalAppTestTempDir(t)
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if got := canonicalAppTestPath(t, alias); got != target {
		t.Fatalf("canonical test path = %q, want %q", got, target)
	}
}
