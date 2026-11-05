package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeSavedPrivateCompatibleHealthyAliasRemainsExact(t *testing.T) {
	f := newPreparedPrivateObservationPair(t)
	persistPreparedPrivateLegacyAlias(t, f)
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	called := 0
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.legacy.Binding(), f.legacy.options, func(context.Context) (string, string, error) { called++; return "", "", errors.New("unused lookup") })
	closeSavedProbeTestClient(t, client, err)
	if err != nil || client == nil || called != 0 || client.RuntimeBinding() != f.legacy.Binding() {
		t.Fatalf("healthy exact alias changed: client=%t calls=%d err=%v", client != nil, called, err)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
	assertPrivateDaemonStartCount(t, f.stateRoot, 1)
}

func TestProbeSavedPrivateCompatibleNeverUsesDThroughIncompleteGeneration(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "alias", true: "migration"}[pending], func(t *testing.T) {
			f := newPreparedPrivateObservationPair(t)
			if pending {
				writePreparedPrivateQueue(t, f, true)
				err := migratePreparedPrivateWriter(t, f, f.successor)
				if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
					t.Fatal(err)
				}
			} else {
				persistPreparedPrivateLegacyAlias(t, f)
			}
			binding := f.legacy.Binding()
			completed := completedPrivateSuccessorTestFixture{clientPath: binding.ClientExecutable, daemonPath: binding.DaemonExecutable}
			damageCompatibleProbePair(t, completed, "missing client")
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, binding, f.legacy.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
			closeSavedProbeTestClient(t, client, err)
			if client != nil || err != nil || called != 0 {
				t.Fatalf("incomplete generation selected D: calls=%d err=%v", called, err)
			}
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
			assertPrivateDaemonStartCount(t, f.stateRoot, 1)
		})
	}
}

func TestProbeSavedPrivateCompatiblePairFaultIsNotUnavailable(t *testing.T) {
	for _, fault := range []string{"stat loop", "directory", "config"} {
		t.Run(fault, func(t *testing.T) {
			f := newCompletedPrivateSuccessorTestFixture(t, true)
			prepareCompatibleProbeOperationalFault(t, f, fault)
			called := 0
			client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
			closeSavedProbeTestClient(t, client, err)
			if err == nil || client != nil || called != 0 {
				t.Fatalf("operational fault became D fallback: calls=%d err=%v", called, err)
			}
		})
	}
}

func prepareCompatibleProbeOperationalFault(t *testing.T, f completedPrivateSuccessorTestFixture, fault string) {
	t.Helper()
	if fault == "config" {
		path := filepath.Join(f.stateRoot, privateSupervisorDirectory, "pueue.yml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { writePrivateSuccessorTestBytes(t, path, data) })
		writePrivateSuccessorTestBytes(t, path, []byte("not: [yaml"))
		return
	}
	damageCompatibleProbePair(t, f, "missing client")
	if fault == "stat loop" {
		if err := os.Symlink(f.clientPath, f.clientPath); err != nil {
			t.Fatal(err)
		}
	} else {
		makePrivateTestDirectory(t, f.clientPath)
	}
	t.Cleanup(func() { removePrivateTestPath(t, f.clientPath) })
}
