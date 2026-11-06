package infrastructure

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// A prior attempt Close fault is injected as immutable owned state. The target
// Store and A's finite child/sole Wait are real; this is not a native B close or
// Pueue placement proof.
func TestResidentOtherRootCleanupFaultStillJoinsActualDaemonWait(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	waits := f.server.waits
	target, err := taskdir.InitStore(filepath.Join(f.root, "other-root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := target.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	child := newOwnerServerOwnedChild(t, f)
	if _, err = child.ensure(t.Context(), f.entry.operation, f.birthContext(t), f.entry.grant, waits); err != nil {
		t.Fatal(err)
	}
	if ready, readErr := bufio.NewReader(child.output).ReadString('\n'); readErr != nil || ready != "ready\n" {
		t.Fatalf("finite child readiness: %q %v", ready, readErr)
	}
	if err = f.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.server = nil // The runtime below owns the drained entry.
	fault := errors.New("prior target attempt close fault")
	resources := &otherPreparationResources{target: target, attempt: &StartupAttempt{closed: true, closeErr: fault}}
	cause := resources.finish(nil)
	finished := make(chan struct{})
	var result error
	t.Cleanup(func() {
		if closeErr := child.gateWrite.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Error(closeErr)
		}
		awaitResidentFixture(t, finished)
	})
	go func() {
		result = finishResidentRuntime(t.Context(), f.entry, waits, cause)
		close(finished)
	}()
	awaitOtherCleanupRegistrationClose(t, waits, finished)
	if err = child.gateWrite.Close(); err != nil {
		t.Fatal(err)
	}
	awaitResidentFixture(t, child.done)
	awaitResidentFixture(t, finished)
	child.verifyReaped(t, t.Context(), waits)
	assertOtherCleanupRetained(t, result, fault, f.entry)
}

func assertOtherCleanupRetained(t *testing.T, result, fault error, entry *ownedResidentEntry) {
	t.Helper()
	var retained *residentEntryCleanupError
	if !errors.Is(result, fault) || !errors.As(result, &retained) || retained.entry != entry || entry.lease == nil {
		t.Fatalf("fault or retained actual entry was lost after sole Wait: %v", result)
	}
}

func awaitOtherCleanupRegistrationClose(t *testing.T, waits *daemonWaitOwner, finished <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		waits.mu.Lock()
		closed := waits.registrationClosed
		waits.mu.Unlock()
		if closed {
			select {
			case <-finished:
				t.Fatal("cleanup fault discarded an actual live child's sole Wait")
			case <-time.After(100 * time.Millisecond):
				return
			}
		}
		select {
		case <-finished:
			t.Fatal("cleanup marker without completion abandoned the daemon Wait")
		case <-deadline.C:
			t.Fatal("cleanup completion never reached the daemon Wait join")
		case <-ticker.C:
		}
	}
}
