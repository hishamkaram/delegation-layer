package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProbeSavedPrivateCompatibleMissingDerivedDirectoryDoesNotPrepare(t *testing.T) {
	for _, name := range []string{"data", "run"} {
		t.Run(name, func(t *testing.T) {
			fixture := newCompletedPrivateSuccessorTestFixture(t, false)
			// The finite fixture has already stopped its generation. Remove only
			// an empty derived directory; never erase live or unknown contents.
			if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, name)); err != nil {
				t.Fatal(err)
			}
			controlMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "probe-control-started")
			fixture.options.Environment = append(os.Environ(), "PRIVATE_CONTROL_MARKER="+controlMarker)
			before := snapshotSavedProbeTestState(t, fixture.stateRoot)
			client, err := ProbeSavedPrivateCompatible(t.Context(), fixture.stateRoot, fixture.saved, fixture.options, nil)
			closeSavedProbeTestClient(t, client, err)
			if client != nil || err != nil {
				t.Fatalf("missing derived directory: client present=%t err=%v", client != nil, err)
			}
			assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
			if _, err := os.Stat(controlMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing derived directory launched a control command: %v", err)
			}
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func TestProbeSavedPrivateCompatibleMissingDirectoryRetainsValidationErrors(t *testing.T) {
	for _, name := range []string{"missing config", "malformed config", "malformed binding", "missing alias config", "malformed alias", "unprotected lease", "symlink config", "symlink runtime", "non-directory runtime"} {
		t.Run(name, func(t *testing.T) {
			fixture := newCompletedPrivateSuccessorTestFixture(t, false)
			if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "data")); err != nil {
				t.Fatal(err)
			}
			want, missing := prepareSavedProbeConfigFailure(t, fixture, name)
			before := snapshotSavedProbeTestState(t, fixture.stateRoot)
			client, err := ProbeSavedPrivateCompatible(t.Context(), fixture.stateRoot, fixture.saved, fixture.options, nil)
			closeSavedProbeTestClient(t, client, err)
			if client != nil || !errors.Is(err, want) || (missing && !errors.Is(err, os.ErrNotExist)) {
				t.Fatalf("invalid dependency: client present=%t err=%v, want %v (missing=%t)", client != nil, err, want, missing)
			}
			assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func prepareSavedProbeConfigFailure(t *testing.T, fixture completedPrivateSuccessorTestFixture, name string) (error, bool) {
	t.Helper()
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	want, missing := ErrConfiguration, false
	switch name {
	case "missing config":
		if err := os.Remove(fixture.saved.ConfigPath); err != nil {
			t.Fatal(err)
		}
		want, missing = ErrBinding, true
	case "malformed config":
		writePrivateSuccessorTestBytes(t, fixture.saved.ConfigPath, []byte("not: [yaml"))
	case "malformed binding":
		writePrivateSuccessorTestBytes(t, privateSupervisorBindingPath(fixture.stateRoot), []byte("{"))
		want = ErrBinding
	case "missing alias config":
		// A record's required file is an unreadable dependency, even though
		// missing derived data is an ordinary needs-preparation observation.
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryBindingRecord{
			SchemaVersion: task.SchemaVersion, Supervisor: fixture.successor,
			ConfigPath: privateRecoveryConfigPath(fixture.stateRoot), ConfigSHA256: fixture.successor.ConfigDigest,
		})
		want, missing = ErrBinding, true
	case "malformed alias":
		writePrivateSuccessorTestBytes(t, privateRecoveryRecordPath(fixture.stateRoot), []byte("{"))
		want = ErrBinding
	case "unprotected lease":
		if err := os.Chmod(filepath.Join(base, "bootstrap.lock"), 0o666); err != nil {
			t.Fatal(err)
		}
	case "symlink config", "symlink runtime", "non-directory runtime":
		path := fixture.saved.ConfigPath
		if name != "symlink config" {
			path = filepath.Join(base, "run")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if name == "non-directory runtime" {
			writePrivateSuccessorTestBytes(t, path, []byte("not a directory"))
			want = syscall.ENOTDIR
		} else if err := os.Symlink(canonicalTemp(t), path); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown invalid probe dependency %q", name)
	}
	return want, missing
}

func TestRecoverPrivatePreparesMissingDerivedDirectoriesUnderExclusiveLock(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	for _, name := range []string{"data", "run"} {
		if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, name)); err != nil {
			t.Fatal(err)
		}
	}
	var recovered *Client
	t.Cleanup(func() {
		if recovered == nil {
			return
		}
		if err := recovered.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		if err := recovered.WaitUnavailable(context.Background()); err != nil {
			t.Error(err)
		}
		if err := recovered.Close(); err != nil {
			t.Error(err)
		}
	})
	recovery, err := AttemptPrivateRecovery(t.Context(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	recovered = recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	if recovered.Binding() != fixture.saved || recovered.RuntimeBinding() != fixture.successor {
		t.Fatal("recovery changed saved or published generation")
	}
	for _, name := range []string{"data", "run"} {
		info, statErr := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, name))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("recovery did not prepare %s: %v", name, statErr)
		}
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}
