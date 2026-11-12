package host

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type postLinkFault struct {
	taskdir.BaseFaultInjector
	fault     error
	remaining atomic.Bool
}

func (f *postLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == stopName(1) && f.remaining.CompareAndSwap(true, false) {
		return f.fault
	}
	return nil
}

func journalFor(t *testing.T) *taskdir.ControlDir {
	t.Helper()
	return journalForStore(t, stateStore(t))
}

func journalForStore(t *testing.T, store *taskdir.Store) *taskdir.ControlDir {
	t.Helper()
	lease, err := AcquireHostLease(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	journal, err := store.OpenSchedulerHostContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return journal
}

func TestHostStopFenceReadbackRetainsAmbiguousPublicationFault(t *testing.T) {
	store := stateStore(t)
	journal := journalForStore(t, store)
	fault := errors.New("stop directory durability fault")
	injector := &postLinkFault{fault: fault}
	injector.remaining.Store(true)
	store.SetFaultInjector(injector)
	record := stopRecord{RootID: store.RootID, Epoch: "0123456789abcdef0123456789abcdef", Host: Identity{PID: 1, StartToken: "exact"}}
	committed, err := publishRecord(t.Context(), journal, stopName(1), record)
	if !committed || !errors.Is(err, fault) {
		t.Fatalf("matching readback/barrier lost original fault: committed=%t err=%v", committed, err)
	}
	var stored stopRecord
	if err = journal.Read(stopName(1), &stored); err != nil || stored != record {
		t.Fatalf("wrong stop fence: %+v %v", stored, err)
	}
	conflict := record
	conflict.Epoch = "fedcba9876543210fedcba9876543210"
	committed, err = publishRecord(t.Context(), journal, stopName(1), conflict)
	if committed || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting fence acknowledged: %t %v", committed, err)
	}
}

func TestHostStopWaitsForAcceptedResponseWriteBeforeShutdown(t *testing.T) {
	fault := errors.New("accepted response was lost with a concrete writer fault")
	assertAcceptedResponseWrite(t, fault, fault)
}

func TestHostLostReplyTransportDoesNotPoisonShutdown(t *testing.T) {
	assertAcceptedResponseWrite(t, io.EOF, nil)
}

func assertAcceptedResponseWrite(t *testing.T, writeFault, wantErr error) {
	t.Helper()
	journal := journalFor(t)
	s := &service{journal: journal, epoch: epochRecord{RootID: "root", Epoch: "0123456789abcdef0123456789abcdef", Sequence: 1, Host: Identity{PID: 1, StartToken: "exact"}}}
	responses := make(chan controlResponse, 1)
	served := make(chan error, 1)
	request := controlRequest{request: wireRequest{Action: "stop"}, reply: responses, served: served}
	type result struct {
		stop bool
		err  error
	}
	done := make(chan result, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		stop, err := s.control(context.Background(), request)
		done <- result{stop, err}
	}()
	t.Cleanup(func() {
		select {
		case served <- nil:
		default:
		}
		receive(t, joined)
	})
	response := receive(t, responses)
	if !response.Accepted || !response.Status.StopAccepted {
		t.Fatalf("durable stop not accepted: %+v", response)
	}
	var saved stopRecord
	if err := journal.Read(stopName(1), &saved); err != nil || saved.Epoch != s.epoch.Epoch {
		t.Fatalf("stop accepted without exact durable fence: %+v %v", saved, err)
	}
	select {
	case early := <-done:
		t.Fatalf("shutdown overtook response writer: %+v", early)
	default:
	}
	served <- writeFault
	finished := receive(t, done)
	if !finished.stop || !errors.Is(finished.err, wantErr) {
		t.Fatalf("lost response lost stop/error authority: %+v", finished)
	}
}
