package app

import (
	"os"
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func canonicalAppTestPath(t *testing.T, path string) string {
	t.Helper()
	return supervisorfixture.CanonicalPath(t, path)
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
	base := supervisorfixture.CanonicalTempDir(t)
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
