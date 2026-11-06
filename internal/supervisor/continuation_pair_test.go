package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func continuationSelectionFixture(t *testing.T) (*helperAdmissionFixture, task.SupervisorRef, AdmissionSelection) {
	t.Helper()
	fixture := newHelperAdmissionFixture(t)
	_, published, _ := installedSelectionFixture(t)
	published.ConfigPath = pueue.PrivateConfigPath(fixture.store.Root)
	published.Endpoint = "unix:" + filepath.Join(fixture.store.Root, "run", "socket")
	selectionExecutable(t, published.ClientExecutable)
	selectionExecutable(t, published.DaemonExecutable)
	writeHistoricalSupervisorBinding(t, fixture.store.Root, published)
	previous := published
	previous.ClientExecutable = filepath.Join(t.TempDir(), "historical", "pueue")
	previous.DaemonExecutable = filepath.Join(filepath.Dir(previous.ClientExecutable), "pueued")
	// This locator exercises selection only; it grants no publication or Start.
	source, err := NewSavedBindingSource(fixture.store.RootID, infrastructure.SavedBindingTaskMetadata, strings.Repeat("a", 32), "", previous)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, published, AdmissionSelection{SavedSupervisor: &previous, ContinuationSource: source}
}

func TestContinuationPairPrefersPublishedInstallation(t *testing.T) {
	fixture, published, selection := continuationSelectionFixture(t)
	client, daemon, err := AdmissionSupervisorExecutablePair(selection, fixture.store.Root, AdmissionOptions{InitialExecutable: "/unavailable-invoking-install/pueue"})
	if err != nil || client != published.ClientExecutable || daemon != published.DaemonExecutable {
		t.Fatalf("published pair replaced: %q %q %v", client, daemon, err)
	}
}

func TestContinuationPairMissingPublishedMemberUsesSelectedInstallation(t *testing.T) {
	fixture, published, selection := continuationSelectionFixture(t)
	if err := os.Remove(published.DaemonExecutable); err != nil {
		t.Fatal(err)
	}
	client, daemon := installedPairFixture(t, "fixture client", "fixture daemon")
	actualClient, actualDaemon, err := AdmissionSupervisorExecutablePair(selection, fixture.store.Root, AdmissionOptions{InitialExecutable: client})
	if err != nil || actualClient != client || actualDaemon != daemon {
		t.Fatalf("missing member fallback: %q %q %v", actualClient, actualDaemon, err)
	}
}

func TestContinuationPairCorruptPublicationRemainsFault(t *testing.T) {
	fixture, _, selection := continuationSelectionFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.store.Root, ".supervisor", "supervisor.identity.json"), []byte("invalid record"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, daemon, err := AdmissionSupervisorExecutablePair(selection, fixture.store.Root, AdmissionOptions{})
	if err == nil || client != "" || daemon != "" {
		t.Fatalf("corrupt publication fell through: %q %q %v", client, daemon, err)
	}
}
