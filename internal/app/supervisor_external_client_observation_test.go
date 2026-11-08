package app

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestStatusExternalClientReplacementObservesPublishedDaemon(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	if complete, err := pueue.PrivateSupervisorMigrationComplete(fixture.root, fixture.predecessor); err != nil || !complete {
		t.Fatalf("external generation is not complete: complete=%t err=%v", complete, err)
	}
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, fixture.clientA, historicalSupervisorClientScript("A", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog)+"\n# compatible installed client update\n")
	before := snapshotObservationPhaseRecords(t, fixture)
	commands := observePhaseStatusCommands(fixture)
	client, err := supervisorcore.RecoverPrivateClient(t.Context(), fixture.store, fixture.predecessor, fixture.options, installation.ExecutableLookup{InitialExecutable: fixture.clientB})
	settleSupervisorProbeTestClient(t, client, err)
	if err != nil || client == nil {
		t.Fatalf("canonical compatible recovery failed: client=%v err=%v", client, err)
	}
	runtime := client.RuntimeBinding()
	if runtime.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || runtime.ClientSHA256 != "" || runtime.DaemonSHA256 != "" || client.Binding() != fixture.predecessor || runtime.DaemonExecutable != fixture.predecessor.DaemonExecutable {
		t.Fatalf("installed client update changed canonical authority: saved=%+v runtime=%+v", fixture.predecessor, runtime)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--root", fixture.root, "status", fixture.req.TaskID, "--json"}, &stdout, &stderr, fixture.dependencies())
	response := decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
	assertObservationPhaseCompatibleResponse(t, code, stdout.Bytes(), stderr.Bytes(), response)
	assertHistoricalTaskEvidence(t, fixture)
	assertObservationPhaseRecords(t, before)
	assertObservationPhaseNoMutation(t, commands())
}
