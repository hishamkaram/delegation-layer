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

type sessionReleaseUnlockFault struct {
	lockFault
	unlockFailed bool
}

func (f *sessionReleaseUnlockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".maintenance.lock" && operation == unix.LOCK_UN && !f.unlockFailed {
		f.unlockFailed = true
		return unix.EIO
	}
	return f.lockFault.OnFlock(path, operation)
}

func TestSessionReleaseRetainsJoinedRunContentionAndUnlockFailure(t *testing.T) {
	s, td, digest := claimedTerminalSession(t)
	fault := &sessionReleaseUnlockFault{lockFault: lockFault{path: ".run.lock", failures: []error{unix.EWOULDBLOCK}}}
	s.SetFaultInjector(fault)
	err := td.ReleaseSession("fixture:test", "release-contention", digest)
	s.SetFaultInjector(nil)
	// Failed unlock conservatively retains the Store's known lease. Its owner
	// explicitly settles it after restoring the native syscall boundary.
	must(t, s.maintLock.Unlock())
	if !errors.Is(err, task.ErrSessionBusy) || !errors.Is(err, task.ErrLockBusy) || !errors.Is(err, unix.EIO) || !fault.unlockFailed {
		t.Fatalf("release lost acquisition or cleanup cause: %v", err)
	}
	if task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) }) {
		t.Fatalf("unlock failure became pure acquisition contention: %v", err)
	}
	if task.AllErrorCauses(errors.Join(&sessionProofLockError{cause: task.ErrLockBusy}, task.ErrSessionBusy), func(cause error) bool {
		return errors.Is(cause, task.ErrLockBusy)
	}) {
		t.Fatal("independent active ownership became acquisition contention")
	}
	must(t, td.ReleaseSession("fixture:test", "release-contention", digest))
}
