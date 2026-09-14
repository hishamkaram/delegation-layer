package antigravity

import (
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

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
