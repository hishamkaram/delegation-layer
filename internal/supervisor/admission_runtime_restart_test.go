package supervisor

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAdmissionRuntimeRestartCannotGrantBootstrapRecovery(t *testing.T) {
	fixture, _ := newWarmContinuationFixture(t)
	client, err := pueue.NewClient(fixture.target, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(nil); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// Historical journal fixture only: there is no daemon or process behind
	// this recorded identity. The public reader validates the pending fence.
	from := pueue.RuntimeRestartReference(fmt.Sprintf("v1:%s:1:%s:%s", fixture.store.RootID, strings.Repeat("1", 32), strings.Repeat("a", 64)))
	type restartFixtureRecord struct {
		From            pueue.RuntimeRestartReference `json:"from"`
		Binding         task.SupervisorRef            `json:"binding"`
		Phase           pueue.RuntimeRestartPhase     `json:"phase"`
		OldPID          int                           `json:"old_pid"`
		OldProcessStart string                        `json:"old_process_start"`
	}
	writeContinuationFixtureRecord(t, filepath.Join(fixture.store.Root, ".supervisor", "bootstrap.pending"), struct {
		SchemaVersion int                  `json:"schema_version"`
		Restart       restartFixtureRecord `json:"runtime_restart"`
	}{SchemaVersion: task.SchemaVersion, Restart: restartFixtureRecord{
		From: from, Binding: fixture.target, Phase: pueue.RuntimeRestartRequested, OldPID: 101, OldProcessStart: "recorded-only",
	}})
	if pending, readErr := pueue.PrivateRuntimeRestartPending(t.Context(), fixture.store.Root); readErr != nil || !pending {
		t.Fatalf("invalid runtime fixture: pending=%t err=%v", pending, readErr)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	for _, endpointProvenLive := range []bool{false, true} {
		err = RejectPrivateAdmissionMigrationWithEndpointProof(t.Context(), fixture.store, client, true, endpointProvenLive)
		if !errors.Is(err, pueue.ErrPrivateRuntimeRestartPending) || errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
			t.Fatalf("pending runtime became bootstrap permission: endpoint_live=%t err=%v", endpointProvenLive, err)
		}
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("runtime refusal changed retained state")
	}
}
