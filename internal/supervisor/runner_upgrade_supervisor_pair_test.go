package supervisor

import (
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func installedPairFixture(t *testing.T, clientContents, daemonContents string) (string, string) {
	t.Helper()
	directory := supervisorfixture.CanonicalTempDir(t)
	client, daemon := filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	supervisorfixture.WriteExecutable(t, client, clientContents)
	supervisorfixture.WriteExecutable(t, daemon, daemonContents)
	return client, daemon
}
