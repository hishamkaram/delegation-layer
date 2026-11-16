package app

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSupervisorStatusRecommendsInstalledPairHandoff(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "present recorded pair", true: "deleted recorded pair"}[missing], func(t *testing.T) {
			fixture := newHistoricalInstallationMigrationFixture(t, missing)
			prepareClosedHistoricalStatusFixture(t, fixture)
			assertInstalledPairHandoff(t, fixture, missing)
		})
	}
}

func prepareClosedHistoricalStatusFixture(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	// These positive status cases model closed journal DATA. Global historical
	// fixtures keep unknown lifetime evidence for refusal and recovery tests.
	recordClosedHistoricalCallbackLifetimeData(t, fixture.td)
	requireSupervisorFixtureClosed(t, fixture.store, fixture.td)
}

func requireSupervisorFixtureClosed(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir) {
	t.Helper()
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), td)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil || !closure.Closed() {
		t.Fatalf("fixture lifetime DATA is not closed: closure=%+v err=%v", closure, err)
	}
	observed, err := store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
	if err != nil || observed.Proof == nil {
		t.Fatalf("fixture closure DATA has no canonical observation: observation=%+v err=%v", observed, err)
	}
}

func assertInstalledPairHandoff(t *testing.T, fixture *historicalSupervisorFixture, missing bool) {
	t.Helper()
	result := supervisorStatus(fixture.root, fixture.dependencies(), newSupervisorCommandResponse("status"))
	if result.code != 0 || result.err != nil {
		t.Fatalf("advisory observation failed: control=%+v err=%v", result.response.SupervisorControl, result.err)
	}
	control := result.response.SupervisorControl
	if control.NextAction != "run_supervisor_ensure_owner" || control.InstallationBinding == nil {
		t.Fatalf("different installed roles hidden: %+v", control)
	}
	if missing && (control.InstallationBinding.Client.RecordedAvailability != "missing" || control.InstallationBinding.Daemon.RecordedAvailability != "missing") {
		t.Fatalf("deleted pair availability hidden: %+v", control.InstallationBinding)
	}
	encoded, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fixture.clientA, fixture.clientB, fixture.daemonA, fixture.daemonB} {
		if strings.Contains(string(encoded), path) {
			t.Fatalf("control leaked executable path: %s", encoded)
		}
	}
}

func TestDoctorReportsDaemonOnlyBindingMismatchWithoutExec(t *testing.T) {
	selected, recordedBundle := doctorBundle(t), doctorBundle(t)
	store := newBareInspectionStore(t)
	binding := testsupport.ContinuationSupervisorRef(store.Root, task.ComputeSHA256([]byte("binding-data-fixture")), "unix:"+filepath.Join(store.Root, "metadata.sock"))
	binding.ClientExecutable = filepath.Join(filepath.Dir(selected), "pueue")
	binding.DaemonExecutable = filepath.Join(filepath.Dir(recordedBundle), "pueued")
	writeSupervisorGenerationTestPrivateGeneration(t, store, binding)
	before := testsupport.SnapshotStore(t, store.Root)
	report, code := observeDoctor(Arguments{Root: store.Root}, Dependencies{}, selected, nil)
	encoded, err := json.Marshal(report.Supervisor)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	var observation struct {
		Relation string `json:"relation"`
	}
	if err = json.Unmarshal(fields["installation_binding"], &observation); err != nil {
		t.Fatalf("missing binding observation: %s %v", encoded, err)
	}
	if code != 2 || observation.Relation != "different" || report.Supervisor.Liveness != "not_checked" {
		t.Fatalf("daemon mismatch not reported passively: %s code=%d", encoded, code)
	}
	if after := testsupport.SnapshotStore(t, store.Root); !maps.Equal(before, after) {
		t.Fatal("doctor changed persisted binding")
	}
	for _, self := range []string{selected, recordedBundle} {
		if _, err = os.Stat(filepath.Join(filepath.Dir(self), "launched")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("doctor executed role: %v", err)
		}
	}
}
