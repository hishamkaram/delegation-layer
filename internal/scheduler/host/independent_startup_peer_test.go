//go:build linux || darwin

package host

import (
	"errors"
	"os"
	"testing"
)

// This exercises the existing native credential owner before its primitive is
// reused by supervisor infrastructure. It does not implement that transport.
func TestIndependentStartupPeerProof(t *testing.T) {
	config := testConfig(t, "hold")
	process := startHostFixture(t, config)
	query := queryFor(t, config)
	eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("published peer: present=%t err=%v", present, err)
	}
	if epoch.Host.PID == os.Getpid() {
		t.Fatal("peer proof requires a separate native process")
	}
	if _, err = contact(t.Context(), query, epoch, "status"); err != nil {
		t.Fatal(err)
	}
	wrongPID := epoch
	wrongPID.Host.PID = os.Getpid()
	if _, err = contact(t.Context(), query, wrongPID, "status"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong peer PID accepted: %v", err)
	}
	wrongStart := epoch
	wrongStart.Host.StartToken += "-different-incarnation"
	if _, err = contact(t.Context(), query, wrongStart, "status"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong peer start identity accepted: %v", err)
	}
	otherConfig := testConfig(t, "hold")
	otherProcess := startHostFixture(t, otherConfig)
	otherQuery := queryFor(t, otherConfig)
	eventuallyStatus(t, otherQuery, func(status Status) bool { return status.Ready })
	otherEpoch, present, err := readLatest(t.Context(), otherQuery.Store, otherQuery.Store.RootID())
	if err != nil || !present {
		t.Fatalf("replacement peer: present=%t err=%v", present, err)
	}
	replaced := epoch
	replaced.Endpoint = otherEpoch.Endpoint
	if _, err = contact(t.Context(), query, replaced, "status"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("different endpoint owner accepted: %v", err)
	}
	finishIndependentPeerFixtures(t, []Query{query, otherQuery}, []*fixtureProcess{process, otherProcess})
	t.Log("native Unix credentials: separate server peer PID/start validated, server authenticated same-UID client, stale incarnation and replacement endpoint refused; different-UID native execution unverified")
}

func finishIndependentPeerFixtures(t *testing.T, queries []Query, processes []*fixtureProcess) {
	t.Helper()
	for _, check := range queries {
		status, observeErr := Observe(t.Context(), check)
		if observeErr != nil || !status.Ready {
			t.Fatalf("refusal damaged live owner: ready=%t err=%v", status.Ready, observeErr)
		}
	}
	for index, check := range queries {
		result, stopErr := Stop(t.Context(), check)
		if stopErr != nil || !result.Stopped {
			t.Fatalf("peer fixture stop: %+v %v", result, stopErr)
		}
		receive(t, processes[index].done)
	}
}
