package control

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

func TestPublishedPrivateProbeRebindsChangedResolution(t *testing.T) {
	fixture, original := supervisorLeaseTestBind(t)
	environment := []string{"HOME=" + filepath.Join(canonicalTemp(t), "new-home"), "PATH=" + os.Getenv("PATH")}
	resolution, err := resolutionContext(environment)
	if err != nil {
		t.Fatal(err)
	}
	var versions atomic.Int64
	options := Options{Resolution: &resolution, Environment: environment, Observer: func(event CommandEvent) {
		if event.Stage == "entry" && slices.Contains(event.Argv, "--version") {
			versions.Add(1)
		}
	}}
	client, ready, err := ProbePrivate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
	supervisorLeaseTestSettleClient(t, client, err)
	if err != nil || !ready || client == nil {
		t.Fatalf("unchanged absolute endpoint rejected after resolution change: ready=%t err=%v", ready, err)
	}
	if client.Binding().Endpoint != original.Binding().Endpoint || versions.Load() != 1 {
		t.Fatalf("changed resolution did not use normal binding: endpoint=%s versions=%d", client.Binding().Endpoint, versions.Load())
	}
}

func TestPublishedPrivateProbeKeepsPublishedPairWhenSuccessorIsStaged(t *testing.T) {
	fixture, original := supervisorLeaseTestBind(t)
	clientData, err := os.ReadFile(fixture.clientPath)
	if err != nil {
		t.Fatal(err)
	}
	daemonData, err := os.ReadFile(fixture.daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	candidateClient := filepath.Join(canonicalTemp(t), "pueue-successor")
	candidateDaemon := filepath.Join(canonicalTemp(t), "pueued-successor")
	writeExecutable(t, candidateClient, string(append(clientData, []byte("\n# staged successor client\n")...)))
	writeExecutable(t, candidateDaemon, string(append(daemonData, []byte("\n# staged successor daemon\n")...)))
	if candidateClient == original.Binding().ClientExecutable || candidateDaemon == original.Binding().DaemonExecutable {
		t.Fatal("successor fixture did not change executable paths")
	}

	var versions atomic.Int64
	var commands atomic.Int64
	options := Options{Observer: func(event CommandEvent) {
		if event.Stage == "entry" {
			commands.Add(1)
			if slices.Contains(event.Argv, "--version") {
				versions.Add(1)
			}
		}
	}}
	client, ready, err := ProbePrivateGroupsWithGate(t.Context(), candidateClient, candidateDaemon, fixture.stateRoot, options, func(context.Context, *Client, QueueSnapshot) (bool, error) {
		return true, nil
	})
	supervisorLeaseTestSettleClient(t, client, err)
	if err != nil || !ready || client == nil {
		t.Fatalf("published generation was not reused with staged successor: client=%v ready=%t err=%v", client, ready, err)
	}
	if client.Binding() != original.Binding() {
		t.Fatalf("staged successor replaced the published generation: got=%+v want=%+v", client.Binding(), original.Binding())
	}
	if commands.Load() != 1 || versions.Load() != 0 {
		t.Fatalf("published group probe launched the wrong supervisor commands: commands=%d versions=%d", commands.Load(), versions.Load())
	}
}

func TestPublishedPrivateProbeReusesOnlyVerifiedExecutableVersion(t *testing.T) {
	for _, changed := range []string{"none", "client", "daemon"} {
		t.Run(changed, func(t *testing.T) {
			assertPublishedPrivateProbeVersion(t, changed)
		})
	}
}

func assertPublishedPrivateProbeVersion(t *testing.T, changed string) {
	t.Helper()
	fixture, _ := supervisorLeaseTestBind(t)
	if changed != "none" {
		path := fixture.clientPath
		if changed == "daemon" {
			path = fixture.daemonPath
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(data, []byte("\n# changed bytes\n")...), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var versions atomic.Int64
	var statuses atomic.Int64
	options := Options{Observer: func(event CommandEvent) {
		if event.Stage != "entry" {
			return
		}
		if slices.Contains(event.Argv, "--version") {
			versions.Add(1)
		}
		if slices.Contains(event.Argv, "status") {
			statuses.Add(1)
		}
	}}
	client, ready, err := ProbePrivate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
	supervisorLeaseTestSettleClient(t, client, err)
	if changed == "none" {
		if err != nil || !ready || statuses.Load() != 1 {
			t.Fatalf("verified probe must observe readiness: ready=%t statuses=%d err=%v", ready, statuses.Load(), err)
		}
	} else if ready {
		t.Fatalf("changed %s accepted: ready=%t err=%v", changed, ready, err)
	}
	if versions.Load() != 0 {
		t.Fatalf("published probe repeated version discovery: %d", versions.Load())
	}
}

func TestBoundControlsDoNotRepeatVersionDiscovery(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	identity := testIdentity()
	writeStatusFor(t, fake, identity.RootID, identity.TaskID, 7, StateQueued)
	restored, err := NewClient(fake.client.Binding(), Options{Environment: fake.environment})
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []*Client{fake.client, restored} {
		if readyErr := client.Ready(t.Context()); readyErr != nil {
			t.Fatal(readyErr)
		}
		if _, snapshotErr := client.Snapshot(t.Context()); snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		observation, observeErr := client.Reconcile(t.Context(), identity)
		if observeErr != nil || !observation.Matched {
			t.Fatalf("bound control did not validate its queue response: observation=%+v err=%v", observation, observeErr)
		}
	}
	invocations := readInvocations(t, fake.logPath)
	if count := countCommand(invocations, "--version"); count != 1 {
		t.Fatalf("expected only the initial version discovery, got %d probes", count)
	}
	if count := countCommand(invocations, "status"); count != 6 {
		t.Fatalf("actual controls were skipped: got %d status commands", count)
	}
}
