package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

func TestAdmissionConsumersRecoverLivePublishedInstalledTransport(t *testing.T) {
	for _, consumer := range []string{"managed-workspace", "admission-helper"} {
		t.Run(consumer, func(t *testing.T) {
			testAdmissionConsumerInstalledTransport(t, consumer)
		})
	}
}

func testAdmissionConsumerInstalledTransport(t *testing.T, consumer string) {
	t.Helper()
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	initial, initialErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, initialErr)
	if initialErr != nil || initial == nil {
		t.Fatalf("initial private admission failed: %v", initialErr)
	}
	active := initial.Binding()
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	candidate := installedGenerationCandidate(t, store, active)
	if err := os.Remove(active.ClientExecutable); err != nil {
		t.Fatal(err)
	}
	options := supervisorAdmissionOptions(deps, deps.SupervisorOptions)
	options.InitialExecutable, options.Routing = candidate.ClientExecutable, nil
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
	receipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	var client *pueue.Client
	var err error
	if consumer == "managed-workspace" {
		runner := filepath.Join(filepath.Dir(candidate.ClientExecutable), "delegate-run")
		selected, prepared, prepareErr := admissioncore.NewWorkspaceRuntime(options).PrepareManaged(t.Context(), store, runner)
		client, err = prepared, prepareErr
		settleSupervisorProbeTestClient(t, client, err)
		if selected != runner {
			t.Fatalf("managed workspace changed selected runner: got=%q want=%q", selected, runner)
		}
	} else {
		client, err = supervisorcore.PrepareAdmissionSupervisorContext(t.Context(), supervisorcore.AdmissionSelection{}, options, store, false, "", true)
		settleSupervisorProbeTestClient(t, client, err)
	}
	if err != nil || client == nil {
		t.Fatalf("public admission consumer refused live installed transport: %v", err)
	}
	if client.Binding() != active || client.RuntimeBinding().ClientExecutable != candidate.ClientExecutable || client.RuntimeBinding().DaemonExecutable != active.DaemonExecutable {
		t.Fatalf("public consumer changed published generation or installed transport: binding=%+v runtime=%+v", client.Binding(), client.RuntimeBinding())
	}
	if closeErr := client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(active.ConfigPath), 1)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, receipt) {
		t.Fatalf("public consumer rewrote scheduling receipt: got=%s want=%s", got, receipt)
	}
}
