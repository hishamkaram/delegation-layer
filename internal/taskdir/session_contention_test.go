package taskdir

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func claimedTerminalSession(t *testing.T) (*Store, *TaskDir, string) {
	t.Helper()
	s := testStore(t)
	td := preparedTask(t, s)
	must(t, td.ClaimSession("fixture:test", "release-contention"))
	permit := consumeStart(t, td)
	must(t, td.WriteRawFiles("answer", ""))
	_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, permit.Release())
	return s, td, collectOK(t, td).EvidenceSHA256
}

func TestSessionReleaseSeparatesMetadataContentionFromActiveOwnership(t *testing.T) {
	s, td, digest := claimedTerminalSession(t)
	const provider, conversation = "fixture:test", "release-contention"
	fault := &lockFault{path: ".session.lock", failures: []error{unix.EWOULDBLOCK}}
	s.SetFaultInjector(fault)
	err := td.ReleaseSession(provider, conversation, digest)
	s.SetFaultInjector(nil)
	must(t, err)
	if fault.calls < 2 {
		t.Fatal("transient lock contention was not exercised")
	}
	path := filepath.Join(td.sessionDirectory(provider, conversation), td.TaskID+".release.json")
	original := readTestFile(t, path)
	must(t, td.ReleaseSession(provider, conversation, digest))
	if string(original) != string(readTestFile(t, path)) {
		t.Fatal("duplicate release rewrote durable evidence")
	}
	successor := preparedTask(t, s)
	must(t, successor.ClaimSession(provider, conversation))
	if err = td.ClaimSession(provider, conversation); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("active successor lost exclusivity: %v", err)
	}
}

func TestSessionReleasePreservesLockIOFailure(t *testing.T) {
	s, td, digest := claimedTerminalSession(t)
	const provider, conversation = "fixture:test", "release-contention"
	s.SetFaultInjector(&lockFault{path: ".session.lock", failures: []error{unix.EIO}})
	err := td.ReleaseSession(provider, conversation, digest)
	s.SetFaultInjector(nil)
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("lock fault hidden: %v", err)
	}
	successor := preparedTask(t, s)
	if err = successor.ClaimSession(provider, conversation); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("failed release freed the session: %v", err)
	}
}
