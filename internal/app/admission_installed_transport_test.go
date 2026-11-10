package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAdmissionConsumersRecoverLivePublishedInstalledTransport(t *testing.T) {
	for _, consumer := range []string{"managed-workspace", "admission-helper"} {
		for _, authorized := range []bool{false, true} {
			name := "refresh"
			if authorized {
				name = "authorized-passive"
			}
			t.Run(consumer+"/"+name, func(t *testing.T) { testAdmissionConsumerInstalledTransport(t, consumer, authorized) })
		}
	}
}

func testAdmissionConsumerInstalledTransport(t *testing.T, consumer string, authorized bool) {
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
	deps.InitialSupervisorExecutable = candidate.ClientExecutable
	deps = withSupervisorProbeRouting(t, deps, owner)
	options := supervisorAdmissionOptions(deps, deps.SupervisorOptions)
	runtime := active
	runtime.ClientExecutable = candidate.ClientExecutable
	if authorized {
		// Passive transport reuse requires a receipt for that exact runtime.
		if err := writePrivateSchedulingFixture(store.Root, runtime, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
			t.Fatal(err)
		}
		options.Routing = nil
	}
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
	if client.Binding() != active || client.RuntimeBinding() != runtime {
		t.Fatalf("public consumer changed published generation or installed transport: binding=%+v runtime=%+v", client.Binding(), client.RuntimeBinding())
	}
	if gateErr := pueue.WithSupervisorLeaseContext(t.Context(), store.Root, func(ctx context.Context) error {
		_, err := supervisorcore.SubmissionGate(store.Root)(ctx, client.RuntimeBinding())
		return err
	}); gateErr != nil {
		t.Fatalf("prepared consumer cannot pass actual submission authorization: %v", gateErr)
	}
	if closeErr := client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(active.ConfigPath), 1)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	assertAdmissionConsumerRuntimeReceipt(t, store.Root, runtime, receipt, authorized)
}

func assertAdmissionConsumerRuntimeReceipt(t *testing.T, root string, runtime task.SupervisorRef, receipt []byte, authorized bool) {
	t.Helper()
	got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(root))
	if authorized && !bytes.Equal(got, receipt) {
		t.Fatalf("passive consumer rewrote scheduling receipt: got=%s want=%s", got, receipt)
	}
	if recorded, err := supervisorcore.OrdinarySchedulingRecorded(root, runtime); err != nil || !recorded {
		t.Fatalf("consumer did not retain runtime scheduling authorization: recorded=%t err=%v", recorded, err)
	}
	if !authorized && bytes.Equal(got, receipt) {
		t.Fatal("resident did not refresh the stale scheduling receipt")
	}
}
