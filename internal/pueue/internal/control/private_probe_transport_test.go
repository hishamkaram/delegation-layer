package control

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPublishedPrivateProbeTransportKeepsLogicalGeneration(t *testing.T) {
	for _, supplied := range []bool{true, false} {
		t.Run(map[bool]string{true: "explicit transport", false: "recorded daemon alias"}[supplied], func(t *testing.T) {
			testPublishedPrivateProbeTransport(t, supplied)
		})
	}
}

func testPublishedPrivateProbeTransport(t *testing.T, supplied bool) {
	t.Helper()
	fixture, original := supervisorLeaseTestBind(t)
	listener := listenPrivateTestEndpoint(t, original.Binding().Endpoint)
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	candidate := filepath.Join(canonicalTemp(t), "pueued")
	writeExecutable(t, candidate, privateDaemonFixture)
	if err := os.Remove(fixture.daemonPath); err != nil {
		t.Fatal(err)
	}
	clientPath, daemonPath := fixture.clientPath, candidate
	if !supplied {
		if err := os.Symlink(candidate, fixture.daemonPath); err != nil {
			t.Fatal(err)
		}
		clientPath, daemonPath = "", ""
	}
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	client, ready, err := ProbePrivateEndpointWithGate(t.Context(), clientPath, daemonPath, fixture.stateRoot, Options{}, func(_ context.Context, observed *Client) (bool, error) {
		return observed.Binding() == original.Binding(), nil
	})
	supervisorLeaseTestSettleClient(t, client, err)
	requirePublishedProbeTransport(t, supplied, original.Binding(), candidate, client, ready, err)
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, fixture.stateRoot)) {
		t.Fatal("transport probe changed published generation state")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func requirePublishedProbeTransport(t *testing.T, supplied bool, original task.SupervisorRef, candidate string, client *Client, ready bool, err error) {
	t.Helper()
	if supplied {
		if err != nil || !ready || client == nil {
			t.Fatalf("installed daemon transport did not observe published endpoint: ready=%t err=%v", ready, err)
		}
		if client.Binding() != original || client.RuntimeBinding().DaemonExecutable != candidate || client.RuntimeBinding().ClientExecutable != original.ClientExecutable {
			t.Fatalf("transport changed logical generation or runtime selection: binding=%+v runtime=%+v", client.Binding(), client.RuntimeBinding())
		}
	} else if client == nil || ready || err != nil || client.Binding() == original {
		t.Fatalf("recorded daemon replacement bypassed the logical-generation gate without supplied transport: client=%t ready=%t err=%v", client != nil, ready, err)
	}
}

func TestAttemptPrivateRecoveryDaemonTransportPreservesPublishedState(t *testing.T) {
	for _, changed := range []string{"missing daemon", "recorded daemon alias"} {
		t.Run(changed, func(t *testing.T) {
			testAttemptPrivateRecoveryDaemonTransport(t, changed)
		})
	}
}

func testAttemptPrivateRecoveryDaemonTransport(t *testing.T, changed string) {
	t.Helper()
	fixture := newCompletedPrivateSuccessorTestFixture(t, true)
	saved := fixture.successor
	directory := filepath.Join(fixture.base, "selected-daemon")
	makePrivateTestDirectory(t, directory)
	candidate := filepath.Join(directory, "pueued")
	writeExecutable(t, candidate, privateDaemonFixture)
	removePrivateTestPath(t, saved.DaemonExecutable)
	if changed == "recorded daemon alias" {
		if err := os.Symlink(candidate, saved.DaemonExecutable); err != nil {
			t.Fatal(err)
		}
	}
	status := statusTestPayload(t, map[string]any{
		"42": statusTestJob(42, "retained queued task", map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	writePrivateSuccessorTestBytes(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), status)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	recovery, err := AttemptPrivateRecovery(t.Context(), fixture.stateRoot, saved, saved.ClientExecutable, candidate, fixture.options)
	client := recovery.Client()
	if client != nil {
		fixture.setCleanupClient(client)
	}
	assertPrivateRecoverySucceeded(t, recovery, err)
	if client.Binding() != saved {
		t.Fatalf("saved recovery changed published logical binding: got=%+v want=%+v", client.Binding(), saved)
	}
	wantRuntime := saved
	wantRuntime.DaemonExecutable = candidate
	if client.RuntimeBinding() != wantRuntime {
		t.Fatalf("saved recovery changed recorded client or selected daemon transport: got=%+v want=%+v", client.RuntimeBinding(), wantRuntime)
	}
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, fixture.stateRoot)) {
		t.Fatal("live saved recovery changed generation, history or queued task state")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}
