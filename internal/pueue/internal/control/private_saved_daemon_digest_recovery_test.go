package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverSavedRejectsExternalSavedDaemonDigestMismatch(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	bound := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if closeErr := bound.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	saved := bound.Binding()

	appendPrivateSupervisorFile(t, fixture.daemonPath, "\n# changed after binding\n")
	if changedDigest := hashPrivateTestExecutable(t, fixture.daemonPath); changedDigest == saved.DaemonSHA256 {
		t.Fatal("test fixture did not change the external saved daemon digest")
	}

	recoveryRecord := privateRecoveryRecordPath(fixture.stateRoot)
	recoveryConfig := privateRecoveryConfigPath(fixture.stateRoot)
	for _, path := range []string{recoveryRecord, recoveryConfig} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery fixture unexpectedly exists before RecoverSaved: %s err=%v", path, err)
		}
	}
	paths := []string{
		saved.ConfigPath,
		privateSupervisorBindingPath(fixture.stateRoot),
		privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)),
		filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready"),
		filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "daemon-started"),
		filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid"),
		fixture.daemonPath,
	}
	before := snapshotPrivateTestFiles(t, paths)

	client, started, err := RecoverSaved(context.Background(), saved, Options{ObservationTimeout: privateTestObservationTimeout})
	if !errors.Is(err, ErrBinding) || !strings.Contains(err.Error(), "saved supervisor daemon identity changed") {
		t.Fatalf("external saved daemon digest mismatch error = %v, want saved-daemon binding rejection", err)
	}
	if client != nil || started {
		t.Fatalf("digest rejection returned recovery client=%t started=%t", client != nil, started)
	}
	for path, want := range before {
		assertPrivateTestFileUnchanged(t, path, want, true)
	}
	for _, path := range []string{recoveryRecord, recoveryConfig} {
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("digest rejection changed recovery state %s: %v", path, err)
		}
	}
}
