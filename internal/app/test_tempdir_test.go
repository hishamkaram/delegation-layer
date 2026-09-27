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
	resolved, err := filepath.EvalSymlinks(directory)
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
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
