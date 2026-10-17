package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestAdmissionConsumersAutoHandoffExternalInspectionJournal(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, *taskdir.Store, Dependencies, task.SupervisorRef)
	}{
		{name: "dispatch", run: runExternalHandoffDispatchAdmission},
		{name: "models", run: runExternalHandoffModelsAdmission},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, deps, current, legacy, externalLog, externalBefore, requestBefore := newAdmissionExternalHandoffFixture(t)
			test.run(t, store, deps, current)
			assertAdmissionExternalHandoff(t, store, legacy, current, externalLog, externalBefore, requestBefore)
		})
	}
}

func TestAdmissionHandoffUsesPublishedPrivateOwnerWhenWarm(t *testing.T) {
	store, deps, initial := newSupervisorProbeTestFixture(t)
	prepared, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	if err != nil || prepared == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", prepared, err)
	}
	privateBinding := prepared.Binding()
	if closeErr := prepared.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if privateBinding != initial.Binding() {
		t.Fatalf("initial admission changed the published private owner: got=%+v want=%+v", privateBinding, initial.Binding())
	}

	_, legacy, externalLog := newExternalInspectionHandoffFixture(t, store)
	externalCalls := readSupervisorProbeTestFile(t, externalLog)
	requestPath := filepath.Join(store.Root, "inspection-group", "request.json")
	requestBefore := readSupervisorProbeTestFile(t, requestPath)

	admitted, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	if err != nil || admitted == nil {
		t.Fatalf("warm private admission did not hand off the external journal: client=%v err=%v", admitted, err)
	}
	t.Cleanup(func() {
		if closeErr := admitted.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if admitted.Binding() != privateBinding {
		t.Fatalf("warm handoff selected a different private owner: got=%+v want=%+v", admitted.Binding(), privateBinding)
	}
	assertSupervisorProbeTestFile(t, externalLog, externalCalls)
	assertSupervisorProbeTestFile(t, requestPath, requestBefore)
	assertExternalHandoffHead(t, store, legacy, privateBinding)
}

func TestAdmissionHandoffBootstrapsExternalOnlyRoot(t *testing.T) {
	templateStore, templateDeps, template := newSupervisorProbeTestFixture(t)
	store := newBareInspectionStore(t)
	_, legacy, externalLog := newExternalInspectionHandoffFixture(t, store)
	externalCalls := readSupervisorProbeTestFile(t, externalLog)

	bundle := testsupport.CanonicalTempDir(t)
	for _, source := range []string{template.Binding().ClientExecutable, template.Binding().DaemonExecutable} {
		data := readSupervisorProbeTestFile(t, source)
		data = []byte(strings.ReplaceAll(string(data), templateStore.RootID, store.RootID))
		writeRunnerFixture(t, filepath.Join(bundle, filepath.Base(source)), string(data))
	}
	deps := templateDeps
	deps.InitialSupervisorExecutable = filepath.Join(bundle, "pueue")
	privateBase := filepath.Join(store.Root, ".supervisor")
	if _, err := os.Stat(privateBase); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external-only fixture already contains private state: %v", err)
	}

	admitted, err := supervisorcore.PrepareAdmissionSupervisor(supervisorAdmissionSelection(Arguments{}), supervisorAdmissionOptions(deps, deps.SupervisorOptions), store, false, "", true)
	if err != nil || admitted == nil {
		t.Fatalf("external-only dispatch admission failed: client=%v err=%v", admitted, err)
	}
	t.Cleanup(func() {
		if closeErr := admitted.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	published, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found {
		t.Fatalf("external-only admission did not publish a private owner: found=%t err=%v", found, err)
	}
	if published != admitted.Binding() {
		t.Fatalf("external-only admission published a different owner: got=%+v want=%+v", published, admitted.Binding())
	}
	complete, err := pueue.PrivateSupervisorMigrationComplete(store.Root, published)
	if err != nil || !complete {
		t.Fatalf("external-only private owner is incomplete: complete=%t err=%v", complete, err)
	}
	assertExternalHandoffHead(t, store, legacy, published)
	assertSupervisorProbeTestFile(t, externalLog, externalCalls)
}

func TestAdmissionHandoffRefusesPrivateInspectionMigration(t *testing.T) {
	store, deps, _ := newSupervisorProbeTestFixture(t)
	prepared, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	if err != nil || prepared == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", prepared, err)
	}
	privateBinding := prepared.Binding()
	if closeErr := prepared.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	legacy := privateBinding
	legacy.ConfigDigest = task.ComputeSHA256([]byte("private-legacy-config"))
	legacy.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("private-legacy-resolved-config"))
	legacy.Endpoint = "unix:" + filepath.Join(store.Root, ".supervisor", "run", "private-legacy.sock")
	openHistoricalInspectionGroup(t, store, legacy)

	_, err = supervisorcore.PrepareAdmissionSupervisor(supervisorAdmissionSelection(Arguments{}), supervisorAdmissionOptions(deps, deps.SupervisorOptions), store, false, "", true)
	if err == nil {
		t.Fatal("private inspection migration was admitted automatically")
	}
	if errors.Is(err, supervisorcore.ErrExternalInspectionHandoffRequired) {
		t.Fatalf("private inspection migration took the external handoff path: %v", err)
	}
	head, migrated, readErr := inspection.ReadGroupHead(t.Context(), store)
	if readErr != nil || migrated || head.Supervisor != legacy {
		t.Fatalf("refused private migration changed the journal: head=%+v migrated=%t err=%v", head, migrated, readErr)
	}
}

func TestAdmissionHandoffConcurrentCallersElectOneOwner(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	_, legacy, externalLog := newExternalInspectionHandoffFixture(t, store)
	externalCalls := readSupervisorProbeTestFile(t, externalLog)

	clients := prepareSupervisorProbeAdmissions(t, store, deps, 16)
	for _, client := range clients {
		if client.Binding() != current.Binding() {
			t.Fatalf("concurrent handoff selected a different private owner: got=%+v want=%+v", client.Binding(), current.Binding())
		}
		if closeErr := client.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	assertExternalHandoffHead(t, store, legacy, current.Binding())
	assertSupervisorProbeTestFile(t, externalLog, externalCalls)
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(current.Binding().ConfigPath), 1)
}

func runExternalHandoffDispatchAdmission(t *testing.T, store *taskdir.Store, deps Dependencies, _ task.SupervisorRef) {
	t.Helper()
	admitted, err := supervisorcore.PrepareAdmissionSupervisor(supervisorAdmissionSelection(Arguments{}), supervisorAdmissionOptions(deps, deps.SupervisorOptions), store, false, "", true)
	if err != nil || admitted == nil {
		t.Fatalf("dispatch admission failed to hand off the external journal: client=%v err=%v", admitted, err)
	}
	t.Cleanup(func() {
		if closeErr := admitted.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}

func runExternalHandoffModelsAdmission(t *testing.T, store *taskdir.Store, deps Dependencies, current task.SupervisorRef) {
	t.Helper()
	deps.RunnerExecutable = current.ClientExecutable
	_, admitted, _, err := prepareModelDiscoverySupervisor(Arguments{}, deps, store, commonprovider.ProfileCandidate{})
	if err != nil || admitted == nil {
		t.Fatalf("models admission failed to hand off the external journal: client=%v err=%v", admitted, err)
	}
	t.Cleanup(func() {
		if closeErr := admitted.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}

func newAdmissionExternalHandoffFixture(t *testing.T) (*taskdir.Store, Dependencies, task.SupervisorRef, inspection.GroupRequestRecord, string, []byte, []byte) {
	t.Helper()
	store, deps, current := newSupervisorProbeTestFixture(t)
	_, legacy, externalLog := newExternalInspectionHandoffFixture(t, store)
	requestPath := filepath.Join(store.Root, "inspection-group", "request.json")
	return store, deps, current.Binding(), legacy, externalLog, readSupervisorProbeTestFile(t, externalLog), readSupervisorProbeTestFile(t, requestPath)
}

func assertAdmissionExternalHandoff(t *testing.T, store *taskdir.Store, legacy inspection.GroupRequestRecord, current task.SupervisorRef, externalLog string, externalBefore, requestBefore []byte) {
	t.Helper()
	assertExternalHandoffHead(t, store, legacy, current)
	assertSupervisorProbeTestFile(t, externalLog, externalBefore)
	requestPath := filepath.Join(store.Root, "inspection-group", "request.json")
	if got := readSupervisorProbeTestFile(t, requestPath); !bytes.Equal(got, requestBefore) {
		t.Fatalf("external inspection request changed: got=%q want=%q", got, requestBefore)
	}
}
