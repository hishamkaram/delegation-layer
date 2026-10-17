package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcileInspectionRetainsLeaseForPendingStatus(t *testing.T) {
	fixture, client := supervisorLeaseTestBind(t)
	base := canonicalTemp(t)
	started, release := filepath.Join(base, "started"), filepath.Join(base, "release")
	defer releasePrivateStatusOnCleanup(t, release)

	faultContext, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.options.Environment = append(os.Environ(), "PRIVATE_STATUS_STARTED="+started, "PRIVATE_STATUS_RELEASE="+release)
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00status\x00") {
			cancel()
		}
	}
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	observation, err := client.ReconcileInspection(faultContext, identity)
	pending := pendingFrom(err)
	if pending == nil || observation.Pending != pending {
		t.Fatalf("reconciliation lost its pending status command: observation=%+v err=%v", observation, err)
	}
	supervisorLeaseTestBlocked(t, fixture.stateRoot, WithBootstrapLock)
	releasePrivateStatus(t, release)
	waitPending(t, pending)
	supervisorLeaseTestEnters(t, fixture.stateRoot, WithBootstrapLock)
}
