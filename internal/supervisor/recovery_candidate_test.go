package supervisor

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecoveryCandidateKeepsRecordedPairBeforeUnusedInventory(t *testing.T) {
	fixture := newSavedControlFixture(t)
	saved := legacyRawControlPair(t, fixture)
	if err := os.WriteFile(saved.ClientExecutable, []byte(parentMigrationClient+"\n# updated client\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.store.Root, ".supervisor", "pueue-pair.json"), []byte("damaged unused inventory"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	client, daemon, err := ResolveRecoveryPrivateSupervisorExecutables(fixture.store.Root, saved, installation.ExecutableLookup{InitialExecutable: "/unavailable-installation/pueue"})
	if err != nil || client != saved.ClientExecutable || daemon != saved.DaemonExecutable {
		t.Fatalf("recorded candidate was replaced or unused inventory read: (%q,%q,%v)", client, daemon, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("candidate selection changed state")
	}
}

func TestRecoveryCandidateUsesCapturedPATHWithoutOverridingInitialChoice(t *testing.T) {
	fixture := newSavedControlFixture(t)
	saved := fixture.target
	saved.ClientExecutable = filepath.Join(fixture.store.Root, "missing-recorded", "pueue")
	saved.DaemonExecutable = filepath.Join(fixture.store.Root, "missing-recorded", "pueued")
	available := supervisorfixture.CanonicalTempDir(t)
	clientPath, daemonPath := filepath.Join(available, "pueue"), filepath.Join(available, "pueued")
	supervisorfixture.WriteExecutable(t, clientPath, "captured PATH client")
	supervisorfixture.WriteExecutable(t, daemonPath, "captured PATH daemon")
	lookup := installation.ExecutableLookup{Environment: []string{"PATH=" + available}}
	client, daemon, err := ResolveRecoveryPrivateSupervisorExecutables(fixture.store.Root, saved, lookup)
	if err != nil || client != clientPath || daemon != daemonPath {
		t.Fatalf("canonical PATH selection changed: (%q,%q,%v)", client, daemon, err)
	}
	lookup.InitialExecutable = filepath.Join(fixture.store.Root, "missing-installation", "pueue")
	client, daemon, err = ResolveRecoveryPrivateSupervisorExecutables(fixture.store.Root, saved, lookup)
	if err == nil || client != "" || daemon != "" {
		t.Fatalf("PATH overrode the selected installation: (%q,%q,%v)", client, daemon, err)
	}
}

// Legacy installations record executable paths outside managed state custody.
// The canonical native/protocol handshake verifies behavior, never executable bytes.
func legacyRawControlPair(t *testing.T, fixture parentMigrationFixture) task.SupervisorRef {
	t.Helper()
	saved := fixture.target
	directory := supervisorfixture.CanonicalTempDir(t)
	supervisorfixture.WriteExecutable(t, filepath.Join(directory, "pueue"), parentMigrationClient+"\n# legacy installed client\n")
	supervisorfixture.WriteExecutable(t, filepath.Join(directory, "pueued"), "#!/bin/sh\nexit 0\n")
	saved.ExecutableIdentity = ""
	saved.ClientSHA256, saved.DaemonSHA256 = strings.Repeat("a", 64), strings.Repeat("b", 64)
	saved.ClientExecutable, saved.DaemonExecutable = filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	return saved
}
