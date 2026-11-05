package supervisor

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecoveryCandidateKeepsRecordedPairBeforeUnusedInventory(t *testing.T) {
	fixture := newSavedControlFixture(t)
	saved := legacyRawControlPair(t, fixture)
	if err := os.WriteFile(saved.ClientExecutable, []byte(parentMigrationClient+"\n# updated client\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.store.Root, ".supervisor", stateSupervisorPairRecordName), []byte("damaged unused inventory"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	client, daemon, err := ResolveRecoveryPrivateSupervisorExecutables(fixture.store.Root, saved, ExecutableLookup{InitialExecutable: "/unavailable-installation/pueue"})
	if err != nil || client != saved.ClientExecutable || daemon != saved.DaemonExecutable {
		t.Fatalf("recorded candidate was replaced or unused inventory read: (%q,%q,%v)", client, daemon, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("candidate selection changed state")
	}
}

func TestRecoveryCandidateKeepsDamagedManagedPairHard(t *testing.T) {
	fixture := newSavedControlFixture(t)
	if err := os.WriteFile(fixture.target.ClientExecutable, []byte("damaged content-addressed fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveRecoveryPrivateSupervisorExecutables(fixture.store.Root, fixture.target, ExecutableLookup{})
	if err == nil {
		t.Fatal("damaged managed pair was selected")
	}
	var unavailable *unavailableBundledPairError
	if errors.As(err, &unavailable) {
		t.Fatalf("managed integrity fault became candidate absence: %v", err)
	}
}

func TestRecoveryCandidateKeepsDamagedInventoryHardAfterMissingRecordedPair(t *testing.T) {
	fixture := newSavedControlFixture(t)
	if err := os.Remove(fixture.target.ClientExecutable); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveRecoveryPrivateSupervisorExecutables(fixture.store.Root, fixture.target, ExecutableLookup{})
	var unavailable *unavailableBundledPairError
	if !errors.Is(err, os.ErrNotExist) || errors.As(err, &unavailable) {
		t.Fatalf("damaged inventory was ignored: %v", err)
	}
}

func TestRecoveryCandidateUsesCapturedPATHWithoutOverridingInitialChoice(t *testing.T) {
	fixture := newSavedControlFixture(t)
	if err := os.Remove(filepath.Join(fixture.store.Root, ".supervisor", stateSupervisorPairRecordName)); err != nil {
		t.Fatal(err)
	}
	saved := fixture.target
	saved.ClientExecutable = filepath.Join(fixture.store.Root, "missing-recorded", "pueue")
	saved.DaemonExecutable = filepath.Join(fixture.store.Root, "missing-recorded", "pueued")
	available := supervisorfixture.CanonicalTempDir(t)
	clientPath, daemonPath := filepath.Join(available, "pueue"), filepath.Join(available, "pueued")
	supervisorfixture.WriteExecutable(t, clientPath, "captured PATH client")
	supervisorfixture.WriteExecutable(t, daemonPath, "captured PATH daemon")
	lookup := ExecutableLookup{Environment: []string{"PATH=" + available}}
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

func TestRecoveryCandidatePreservesJoinedBundleFaults(t *testing.T) {
	original := errors.Join(pueue.ErrConfiguration, os.ErrNotExist, os.ErrPermission)
	classified := classifyUnavailableBundledPair(original)
	var unavailable *unavailableBundledPairError
	if errors.As(classified, &unavailable) || !errors.Is(classified, os.ErrPermission) || !errors.Is(classified, os.ErrNotExist) {
		t.Fatalf("joined bundle fault became unavailable: %v", classified)
	}
	missing := classifyUnavailableBundledPair(errors.Join(pueue.ErrConfiguration, os.ErrNotExist))
	if !errors.As(missing, &unavailable) || !errors.Is(missing, os.ErrNotExist) || !errors.Is(missing, pueue.ErrConfiguration) {
		t.Fatalf("genuine bundle absence lost diagnostic causes: %v", missing)
	}
}

// Legacy installations record executable paths outside managed state custody.
// Their actual bytes are verified later by the canonical binding handshake.
func legacyRawControlPair(t *testing.T, fixture parentMigrationFixture) task.SupervisorRef {
	t.Helper()
	saved := fixture.target
	directory := supervisorfixture.CanonicalTempDir(t)
	for _, member := range []struct{ source, name string }{{saved.ClientExecutable, "pueue"}, {saved.DaemonExecutable, "pueued"}} {
		data, err := os.ReadFile(member.source)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(directory, member.name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	saved.ClientExecutable, saved.DaemonExecutable = filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	return saved
}
