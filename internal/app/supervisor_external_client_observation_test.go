package app

import (
	"bytes"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

func TestStatusExternalClientReplacementObservesPublishedDaemon(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	if complete, err := pueue.PrivateSupervisorMigrationComplete(fixture.root, fixture.predecessor); err != nil || !complete {
		t.Fatalf("external generation is not complete: complete=%t err=%v", complete, err)
	}
	original := readSupervisorProbeTestFile(t, fixture.clientA)
	writeRunnerFixture(t, fixture.clientA, string(original)+"\n# compatible client rebuild\n")
	t.Cleanup(func() {
		if err := os.WriteFile(fixture.clientA, original, 0o755); err != nil {
			t.Error(err)
		}
	})
	before := snapshotObservationPhaseRecords(t, fixture)
	commands := observePhaseStatusCommands(fixture)
	client, err := supervisorcore.NewClient(t.Context(), fixture.root, fixture.predecessor, fixture.options, true, fixture.clientB, openNativeSupervisorStore)
	settleSupervisorProbeTestClient(t, client, err)
	if err != nil || client == nil {
		t.Fatalf("canonical compatible recovery failed: client=%v err=%v", client, err)
	}
	runtime := client.RuntimeBinding()
	if runtime.ClientSHA256 == fixture.predecessor.ClientSHA256 || runtime.DaemonExecutable != fixture.predecessor.DaemonExecutable || runtime.DaemonSHA256 != fixture.predecessor.DaemonSHA256 {
		t.Fatalf("fixture did not isolate client provenance: saved=%+v runtime=%+v", fixture.predecessor, runtime)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--root", fixture.root, "status", fixture.req.TaskID, "--json"}, &stdout, &stderr, fixture.dependencies())
	response := decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
	assertObservationPhaseCompatibleResponse(t, code, stdout.Bytes(), stderr.Bytes(), response)
	assertHistoricalTaskEvidence(t, fixture)
	assertObservationPhaseRecords(t, before)
	assertObservationPhaseNoMutation(t, commands())
}
