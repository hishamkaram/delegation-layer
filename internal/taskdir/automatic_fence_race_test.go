package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAutomaticFenceBusyLeasesDoNotWriteRefusal(t *testing.T) {
	for _, held := range []string{"admission", "run", "lifetime"} {
		t.Run(held, func(t *testing.T) {
			store := testStore(t)
			td := supervisorTask(t, store)
			peer, err := store.OpenTask(td.TaskID)
			must(t, err)
			t.Cleanup(func() { must(t, peer.Close()) })
			var release func() error
			switch held {
			case "admission":
				must(t, peer.admissionLock.LockEXNonblocking())
				release = peer.admissionLock.Unlock
			case "run":
				// This is the no-start window: a live runner owner without provider.start.
				must(t, peer.runLock.LockEXNonblocking())
				release = peer.runLock.Unlock
			case "lifetime":
				owner, ownerErr := peer.AcquireLifetimeOwner(t.Context())
				must(t, ownerErr)
				release = owner.Release
			}
			t.Cleanup(func() { must(t, release()) })
			proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
			must(t, err)
			if proven || digest != "" {
				t.Fatalf("held %s granted proof", held)
			}
			rows, err := td.ReadStopRecords()
			must(t, err)
			if len(rows) != 0 {
				t.Fatalf("held %s was stopped", held)
			}
			must(t, release())
			proven, _, err = td.FenceUnlaunchedTaskContext(t.Context())
			must(t, err)
			if !proven {
				t.Fatalf("released %s still blocked", held)
			}
		})
	}
}

type automaticRefusalGate struct {
	BaseFaultInjector
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	afterLink bool
}

func (g *automaticRefusalGate) OnLink(_, destination string) error {
	if !g.afterLink && strings.HasSuffix(destination, stopRequestSuffix) {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return nil
}

func (g *automaticRefusalGate) OnAfterLinkDirBarrier(destination string) error {
	if g.afterLink && strings.HasSuffix(destination, stopRequestSuffix) {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return nil
}

type automaticFenceResult struct {
	proven bool
	digest string
	err    error
}

func TestAutomaticFenceKeepsLifetimeAndRunThroughWriteAndProof(t *testing.T) {
	for _, phase := range []string{"before-write", "after-write"} {
		t.Run(phase, func(t *testing.T) { assertAutomaticFenceCriticalSection(t, phase == "after-write") })
	}
}

func assertAutomaticFenceCriticalSection(t *testing.T, afterLink bool) {
	t.Helper()
	store := testStore(t)
	td := supervisorTask(t, store)
	peer, err := store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, peer.Close()) })
	gate := &automaticRefusalGate{entered: make(chan struct{}), release: make(chan struct{}), afterLink: afterLink}
	store.SetFaultInjector(gate)
	result := make(chan automaticFenceResult, 1)
	done := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(gate.release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("automatic fence did not join after gate release")
		}
	})
	go func() {
		defer close(done)
		proven, digest, fenceErr := td.FenceUnlaunchedTaskContext(t.Context())
		result <- automaticFenceResult{proven, digest, fenceErr}
	}()
	select {
	case <-gate.entered:
	case finished := <-result:
		t.Fatalf("fence returned before the write gate: %+v", finished)
	case <-time.After(5 * time.Second):
		t.Fatal("fence did not reach the write gate")
	}
	assertAutomaticFenceExcludesOtherOwners(t, peer)
	id := task.ComputeSHA256([]byte(task.StopCauseAutomaticAbandonment + ":" + peer.TaskID))[:32]
	path := filepath.Join(peer.Dir, "stop", id+stopRequestSuffix)
	if afterLink {
		_, err = os.Lstat(path)
		must(t, err)
	} else {
		testAbsent(t, path)
	}
	release.Do(func() { close(gate.release) })
	finished := <-result
	must(t, finished.err)
	if !finished.proven || finished.digest == "" {
		t.Fatalf("fence result: %+v", finished)
	}
	if _, err = peer.PrepareStart(0); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("late Start was not fenced: %v", err)
	}
	_, meta, err := peer.PreparedRecords()
	must(t, err)
	if _, err = peer.PrepareSubmission(meta.SupervisorConfig); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("late submission was not fenced: %v", err)
	}
	assertNoFabricatedExecution(t, td)
}

func assertAutomaticFenceExcludesOtherOwners(t *testing.T, peer *TaskDir) {
	t.Helper()
	owner, err := peer.TryAcquireLifetimeOwnerContext(t.Context())
	if owner != nil {
		must(t, owner.Release())
	}
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("lifetime owner escaped pre-write fence: %v", err)
	}
	if permit, startErr := peer.PrepareStart(0); !errors.Is(startErr, task.ErrLockBusy) {
		if permit != nil {
			must(t, permit.Release())
		}
		t.Fatalf("Start escaped pre-write run fence: %v", startErr)
	}
	_, meta, err := peer.PreparedRecords()
	must(t, err)
	if permit, submitErr := peer.PrepareSubmission(meta.SupervisorConfig); !errors.Is(submitErr, task.ErrLockBusy) {
		if permit != nil {
			must(t, permit.Release())
		}
		t.Fatalf("submission escaped pre-write admission fence: %v", submitErr)
	}
}
