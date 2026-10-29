package control

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordPrivateTerminalEventNeverProvisionsMissingEventLock: a callback
// that still finds its ticket after disposal removed the event lock must not
// manufacture callback state. It reports no recorded candidate, so the caller
// takes the conservative full-wake path.
func TestRecordPrivateTerminalEventNeverProvisionsMissingEventLock(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	lockPath := filepath.Join(base, privateTerminalEventLockName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}

	recorded, err := RecordPrivateTerminalEventForMaintenance(t.Context(), fixture.stateRoot, 42, "0")
	if err != nil || recorded {
		t.Fatalf("missing event lock recorded=%t err=%v, want no candidate", recorded, err)
	}
	for _, path := range []string{lockPath, filepath.Join(base, privateTerminalEventRecordDir)} {
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing event lock callback created %s: %v", path, err)
		}
	}
}

// TestRecordPrivateTerminalEventRecoversStagesUnderExistingLock: the callback
// recorder recovers interrupted publisher stages under the existing event
// lock before its locked ticket recheck and event write.
func TestRecordPrivateTerminalEventRecoversStagesUnderExistingLock(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventRecordDir)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	stagePath := filepath.Join(directory, privateRecordStagePrefix+strings.Repeat("c", 32))
	if err := os.WriteFile(stagePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	recorded, err := RecordPrivateTerminalEventForMaintenance(t.Context(), fixture.stateRoot, 42, "0")
	if err != nil || !recorded {
		t.Fatalf("ticketed callback recorded=%t err=%v", recorded, err)
	}
	if _, err = os.Lstat(stagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("callback recorder retained interrupted stage: %v", err)
	}
	event, found, err := client.ReadPrivateTerminalEvent(t.Context(), 42)
	if err != nil || !found || event.NumericTaskID != 42 || !event.Succeeded {
		t.Fatalf("recorded event=%+v found=%t err=%v", event, found, err)
	}
}
