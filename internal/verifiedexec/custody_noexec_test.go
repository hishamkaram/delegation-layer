package verifiedexec

// A copied executable must run from a filesystem that allows it. These tests
// need a real noexec mount that already exists; they never create one.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// existingNoexecDir returns a fresh directory on a mount the platform reports
// as noexec, or skips with the recorded reason.
func existingNoexecDir(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{os.Getenv(proofOtherFSEnv), "/dev/shm"} {
		if candidate == "" {
			continue
		}
		forbids, err := filesystemForbidsExec(candidate)
		if err != nil || !forbids {
			continue
		}
		dir, err := os.MkdirTemp(candidate, "custody-noexec-")
		if err != nil {
			continue
		}
		t.Cleanup(func() {
			if removeErr := os.RemoveAll(dir); removeErr != nil {
				t.Errorf("remove: %v", removeErr)
			}
		})
		return dir
	}
	t.Skipf("no existing writable noexec mount (checked %s and /dev/shm); this test never creates one", proofOtherFSEnv)
	return ""
}

func requireNoViewLeft(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, "delegation-layer-verified-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("a refused launch left %v behind (%v)", left, err)
	}
}

func TestPrivateNativeEntryOnANoexecTmpdirIsRefusedBeforeLaunch(t *testing.T) {
	tmp := existingNoexecDir(t)
	entry, digest := selfNative(t, filepath.Join(scratch(t), "fixture"))
	before := fileDigest(t, entry)
	command, err := NewCommand(entry, digest, []string{"TMPDIR=" + tmp}, helperArg)
	if command != nil {
		t.Cleanup(func() {
			if closeErr := command.Close(); closeErr != nil {
				t.Errorf("close: %v", closeErr)
			}
		})
	}
	if !errors.Is(err, errViewNoExec) {
		t.Fatalf("a native entry with a noexec TMPDIR gave %v; want %v", err, errViewNoExec)
	}
	for _, want := range []string{tmp, "TMPDIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q", err, want)
		}
	}
	requireNoViewLeft(t, tmp)
	if fileDigest(t, entry) != before {
		t.Fatal("the original bytes changed")
	}
}

func TestPrivateScriptWhoseInterpreterIsCopiedOnANoexecTmpdirIsRefused(t *testing.T) {
	tmp := existingNoexecDir(t)
	f := newEntryFixture(t, scratch(t))
	command, err := NewCommand(f.script, f.digest, []string{"TMPDIR=" + tmp})
	if command != nil {
		t.Cleanup(func() {
			if closeErr := command.Close(); closeErr != nil {
				t.Errorf("close: %v", closeErr)
			}
		})
	}
	if !errors.Is(err, errViewNoExec) {
		t.Fatalf("a script with a copied interpreter on a noexec TMPDIR gave %v; want %v", err, errViewNoExec)
	}
	requireNoViewLeft(t, tmp)
}

func TestPoolViewsOnAnExecRootAreUnaffectedByANoexecTmpdir(t *testing.T) {
	tmp := existingNoexecDir(t)
	rig := newPoolRig(t)
	entry, digest := selfNative(t, filepath.Join(rig.dir, "fixture"))
	t.Setenv("TMPDIR", tmp)
	command := rig.binder("a", CommandRequest{Executable: entry, SHA256: digest, Environment: []string{"TMPDIR=" + tmp}, Args: []string{helperArg}})
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatal(err)
	}
	if report.Marker != digest {
		t.Fatalf("pooled launch ran %s, want %s", report.Marker, digest)
	}
}
