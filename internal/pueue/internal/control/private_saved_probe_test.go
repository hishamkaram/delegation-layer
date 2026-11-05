package control

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These finite command fixtures exercise control policy and filesystem effects;
// they are not proof of a native Pueue daemon or resident authority.
func TestProbeSavedPrivateAttachesCompletedSuccessorWithoutWrites(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, true)
	before := snapshotSavedProbeTestState(t, fixture.stateRoot)
	client, err := ProbeSavedPrivate(t.Context(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	closeSavedProbeTestClient(t, client, err)
	if err != nil || client == nil {
		t.Fatalf("warm successor: client present=%t err=%v", client != nil, err)
	}
	if client.Binding() != fixture.saved || client.RuntimeBinding() != fixture.successor {
		t.Fatal("warm probe changed saved or current generation")
	}
	assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestProbeSavedPrivateNotReadyNeverRecovers(t *testing.T) {
	for _, missingPair := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "missing pair"}[missingPair], func(t *testing.T) {
			fixture := newCompletedPrivateSuccessorTestFixture(t, false)
			if missingPair {
				removePrivateTestPath(t, filepath.Join(fixture.base, "completed"))
			}
			before := snapshotSavedProbeTestState(t, fixture.stateRoot)
			client, err := ProbeSavedPrivate(t.Context(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
			closeSavedProbeTestClient(t, client, err)
			if client != nil || err != nil {
				t.Fatalf("not-ready probe: client present=%t err=%v", client != nil, err)
			}
			assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func TestProbeSavedPrivateErrorsNeverRecover(t *testing.T) {
	for _, name := range []string{"missing config", "malformed config", "malformed binding", "uncertain status", "unrelated saved", "canceled"} {
		t.Run(name, func(t *testing.T) {
			fixture := newCompletedPrivateSuccessorTestFixture(t, true)
			ctx, saved, want := prepareSavedProbeTestDecision(t, fixture, name)
			before := snapshotSavedProbeTestState(t, fixture.stateRoot)
			client, err := ProbeSavedPrivate(ctx, fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, fixture.options)
			closeSavedProbeTestClient(t, client, err)
			if client != nil || !errors.Is(err, want) {
				t.Fatalf("probe: client present=%t err=%v, want %v", client != nil, err, want)
			}
			assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
		})
	}
}

func prepareSavedProbeTestDecision(t *testing.T, fixture completedPrivateSuccessorTestFixture, name string) (context.Context, task.SupervisorRef, error) {
	t.Helper()
	saved, ctx := fixture.saved, t.Context()
	configBytes, readErr := os.ReadFile(saved.ConfigPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	t.Cleanup(func() {
		if restoreErr := os.WriteFile(saved.ConfigPath, configBytes, 0o600); restoreErr != nil {
			t.Error(restoreErr)
		}
	})
	want := ErrBinding
	switch name {
	case "missing config":
		if err := os.Remove(saved.ConfigPath); err != nil {
			t.Fatal(err)
		}
	case "malformed config":
		want = ErrConfiguration
		writePrivateSuccessorTestBytes(t, saved.ConfigPath, []byte("not: [yaml"))
	case "malformed binding":
		writePrivateSuccessorTestBytes(t, privateSupervisorBindingPath(fixture.stateRoot), []byte("{"))
	case "uncertain status":
		writePrivateSuccessorTestBytes(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), []byte("{"))
	case "unrelated saved":
		saved.ConfigDigest = task.ComputeSHA256([]byte("unrelated generation"))
		want = nil
	case "canceled":
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		ctx = canceled
		want = context.Canceled
	}
	return ctx, saved, want
}

func TestProbeSavedPrivateDoesNotCreateMissingLease(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "bootstrap.lock")); err != nil {
		t.Fatal(err)
	}
	before := snapshotSavedProbeTestState(t, fixture.stateRoot)
	client, err := ProbeSavedPrivate(t.Context(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	closeSavedProbeTestClient(t, client, err)
	if client != nil || err != nil {
		t.Fatalf("missing lease: client present=%t err=%v", client != nil, err)
	}
	assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
}

func closeSavedProbeTestClient(t *testing.T, client *Client, probeErr error) {
	t.Helper()
	if closeErr := client.CloseAfterPending(probeErr); closeErr != nil {
		t.Errorf("close warm client: %v", closeErr)
	}
}

func assertSavedProbeTestUnchanged(t *testing.T, root string, before map[string]privateSuccessorTestFile) {
	t.Helper()
	if !maps.Equal(before, snapshotSavedProbeTestState(t, root)) {
		t.Fatal("warm probe changed root entries, bytes, modes, or modification times")
	}
}

func TestProbeSavedPrivateCloseFaultCannotBecomeNotReady(t *testing.T) {
	closeFault := errors.New("owned client cleanup fault")
	observedFault := errors.New("uncertain observation")
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "not ready", true: "observation fault"}[uncertain], func(t *testing.T) {
			cleanupCount := 0
			client := &Client{cleanup: func() error { cleanupCount++; return closeFault }}
			var probeErr error
			if uncertain {
				probeErr = observedFault
			}
			// An observation fault releases its client through the same transition
			// finalizer used by observePrivateWithGate, retaining original close causes.
			if uncertain {
				probeErr = failPrivateTransitionClient(client, PrivateEndpointObservation{}, probeErr)
				client = nil
			}
			result, err := finishSavedPrivateProbe(task.SupervisorRef{}, client, false, probeErr)
			if result != nil || !errors.Is(err, closeFault) {
				t.Fatalf("cleanup fault became clean not-ready: %v", err)
			}
			if uncertain && !errors.Is(err, observedFault) {
				t.Fatalf("lost observation fault: %v", err)
			}
			if cleanupCount != 1 {
				t.Fatalf("cleanup count=%d", cleanupCount)
			}
		})
	}
}

func TestProbeSavedPrivateAbsentRootDoesNotInitialize(t *testing.T) {
	parent := canonicalTemp(t)
	root := filepath.Join(parent, "missing")
	saved := validBindingForTest()
	saved.ConfigPath = PrivateConfigPath(root)
	before := snapshotSavedProbeTestState(t, parent)
	client, err := ProbeSavedPrivate(t.Context(), root, saved, saved.ClientExecutable, saved.DaemonExecutable, Options{})
	closeSavedProbeTestClient(t, client, err)
	if client != nil || !errors.Is(err, ErrBinding) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent root: client present=%t err=%v", client != nil, err)
	}
	assertSavedProbeTestUnchanged(t, parent, before)
}

func TestProbeSavedPrivateRefusedSocketIsNotRecoveryPermission(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	before := snapshotSavedProbeTestState(t, fixture.stateRoot)
	client, err := ProbeSavedPrivate(t.Context(), fixture.stateRoot, fixture.client.Binding(), fixture.clientPath, fixture.daemonPath, fixture.client.options)
	closeSavedProbeTestClient(t, client, err)
	if client != nil || err != nil {
		t.Fatalf("refused endpoint: client present=%t err=%v", client != nil, err)
	}
	fixture.assertSocketPreserved(t)
	assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

// A socket has identity/mode but no readable file bytes. All regular control
// files retain the same byte/mode/time comparison as successor observer tests;
// the refused-socket test also checks the actual native inode independently.
func snapshotSavedProbeTestState(t *testing.T, root string) map[string]privateSuccessorTestFile {
	t.Helper()
	files := make(map[string]privateSuccessorTestFile)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := privateSuccessorTestFile{mode: info.Mode(), modified: info.ModTime()}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			file.contents = string(data)
		}
		files[path] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
