package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRunnerRecognizesMalformedInfrastructureOwnerBeforeTaskArguments(t *testing.T) {
	for _, args := range [][]string{{"--infrastructure-owner"}, {"--infrastructure-owner=invalid"}, {"--root", "/", "--infrastructure-owner"}} {
		var stderr bytes.Buffer
		code := RunRunner(args, io.Discard, &stderr, Dependencies{})
		if code != 1 || !strings.Contains(stderr.String(), "Infrastructure owner unavailable.") {
			t.Fatalf("owner mode fell through to task argument parsing: code %d, %q", code, stderr.String())
		}
	}
}

func TestInfrastructureGuardianCleanupRetainsActualBorrowedStore(t *testing.T) {
	store, settle, cause := joinedObservationFailure(t, "lifetime")
	result := closeGuardianStoreAfterCleanup(store, cause)
	if !errors.Is(result, cause) || errors.Is(result, task.ErrLockBusy) {
		t.Fatalf("guardian attempted Store.Close behind unresolved admission cleanup: %v", result)
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("guardian closed borrowed Store: %v", err)
	}
	store.SetFaultInjector(nil)
	if err := settle(); err != nil {
		t.Fatal(err)
	}
	result = closeGuardianStoreAfterCleanup(store, cause)
	if !errors.Is(result, cause) {
		t.Fatalf("guardian lost settled cleanup cause: %v", result)
	}
	if _, err := store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("guardian retained settled Store: %v", err)
	}
}
