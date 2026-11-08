package installation

import (
	"os"
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func selectionExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture, never executed or read"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func installedPairFixture(t *testing.T, clientContents, daemonContents string) (string, string) {
	t.Helper()
	directory := supervisorfixture.CanonicalTempDir(t)
	client, daemon := filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	supervisorfixture.WriteExecutable(t, client, clientContents)
	supervisorfixture.WriteExecutable(t, daemon, daemonContents)
	return client, daemon
}
