package control

import (
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

func TestResidentCloseJoinsPendingOwnerBeforeCleanup(t *testing.T) {
	synctest.Test(t, testResidentCloseJoinsPendingOwner)
}

func testResidentCloseJoinsPendingOwner(t *testing.T) {
	cleanupErr := errors.New("owned cleanup failed")
	client := &Client{
		options:   Options{WaitForPendingClose: true},
		workOwner: &clientWorkOwner{}, ownsWorkOwner: true,
		cleanup: func() error { return cleanupErr },
	}
	finish, err := client.beginCommandWork()
	if err != nil {
		t.Fatal(err)
	}
	pending := transport.NewPending([]string{"enqueue"})
	release := sync.OnceFunc(func() {
		transport.CompletePending(pending, transport.CommandResult{})
		finish()
	})
	defer release()
	closed := make(chan error, 1)
	go func() { closed <- client.CloseAfterPending(&InFlightError{Pending: pending}) }()
	synctest.Wait()
	// The admitted owner has not completed. Even with a pending error the
	// resident caller must retain its slot/lease until that owner settles.
	select {
	case err := <-closed:
		t.Fatalf("resident close returned before pending work settled: %v", err)
	default:
	}
	release()
	synctest.Wait()
	if err := <-closed; !errors.Is(err, cleanupErr) {
		t.Fatalf("close lost the owned cleanup error: %v", err)
	}
}
