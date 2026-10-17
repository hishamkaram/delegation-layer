package admission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestEventlessFullInspectionRecoveryRetainsUnsafeRows(t *testing.T) {
	for _, fault := range eventlessFullRecoveryFaults() {
		t.Run(fault.name, func(t *testing.T) {
			runEventlessFullRecoveryFault(t, fault)
		})
	}
}

func runEventlessFullRecoveryFault(t *testing.T, fault eventlessFullRecoveryFault) {
	t.Helper()
	fixture := newPendingPublicationFixture(t)
	fault.inject(t, fixture)
	ticketPath := filepath.Join(fixture.store.Root, ".supervisor", "terminal-event-tickets", "8.json")
	ticket, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	requireEventlessFullRecovery(t, fixture)
	if err := pueue.RequestPrivateQueueMaintenance(t.Context(), fixture.store.Root); err != nil {
		t.Fatal(err)
	}
	if !fixture.client.RunRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal("eventless full recovery did not start")
	}
	admissionfixture.WaitForPrivateQueueMaintenanceIdle(t, t.Context(), fixture.client)
	if err := fixture.client.Close(); err != nil {
		t.Fatal(err)
	}
	requireSafeFullRecoveryRemoval(t, fixture, string(ticket))
	requirePublicationJournalSettled(t, fixture.store.Root)
	if journal := readPublicationJournal(t, fixture.store.Root); journal.FullRequested == 0 {
		t.Fatalf("recovery did not request a full pass: %+v", journal)
	}
	if fixture.launches.Load() != 0 {
		t.Fatal("eventless recovery invoked a terminal callback launcher")
	}
	if fault.missingRecord != "" {
		path := filepath.Join(fixture.store.Root, "inspections", fixture.observations[1].Identity.TaskID, fault.missingRecord)
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("unsafe inspection recreated %s: %v", fault.missingRecord, err)
		}
	}
}

type eventlessFullRecoveryFault struct {
	name          string
	inject        func(*testing.T, pendingPublicationFixture)
	missingRecord string
}

func eventlessFullRecoveryFaults() []eventlessFullRecoveryFault {
	return []eventlessFullRecoveryFault{
		{name: "receipt ID mismatch", inject: mismatchFullRecoveryReceipt},
		{name: "duplicate inspection label", inject: duplicateFullRecoveryLabel},
		{
			name: "missing completion", missingRecord: "completion.json",
			inject: func(t *testing.T, fixture pendingPublicationFixture) {
				removeFullRecoveryRecord(t, fixture, "completion.json")
			},
		},
		{name: "unclosed lifetime with live owner", inject: holdUnclosedFullRecoveryLifetime, missingRecord: "lifetime-termination.json"},
		{name: "running incomplete inspection", inject: writeRunningFullRecoveryInspection, missingRecord: "completion.json"},
	}
}

func mismatchFullRecoveryReceipt(t *testing.T, fixture pendingPublicationFixture) {
	t.Helper()
	jobs := []pueue.Job{fixture.observations[0].Job, fixture.observations[1].Job}
	jobs[1].ID = 18
	writeTerminalEventStatuses(t, fixture.store, jobs)
}

func duplicateFullRecoveryLabel(t *testing.T, fixture pendingPublicationFixture) {
	t.Helper()
	duplicate := fixture.observations[1].Job
	duplicate.ID = 9
	writeTerminalEventStatuses(t, fixture.store, []pueue.Job{
		fixture.observations[0].Job, fixture.observations[1].Job, duplicate,
	})
}

func removeFullRecoveryRecord(t *testing.T, fixture pendingPublicationFixture, name string) {
	t.Helper()
	path := filepath.Join(fixture.store.Root, "inspections", fixture.observations[1].Identity.TaskID, name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func holdUnclosedFullRecoveryLifetime(t *testing.T, fixture pendingPublicationFixture) {
	t.Helper()
	operation, err := inspection.LoadOperationContext(t.Context(), fixture.store, fixture.observations[1].Identity.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	owner, err := operation.TryAcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	removeFullRecoveryRecord(t, fixture, "lifetime-termination.json")
}

func writeRunningFullRecoveryInspection(t *testing.T, fixture pendingPublicationFixture) {
	t.Helper()
	removeFullRecoveryRecord(t, fixture, "completion.json")
	path := filepath.Join(fixture.store.Root, ".supervisor", "terminal-event-status.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status := decodeFullRecoveryStatusObject(t, data)
	jobs := decodeFullRecoveryStatusObject(t, status["tasks"])
	job := decodeFullRecoveryStatusObject(t, jobs["8"])
	job["status"], err = json.Marshal(terminalEventRunningState())
	if err != nil {
		t.Fatal(err)
	}
	jobs["8"] = marshalFullRecoveryStatusObject(t, job)
	status["tasks"] = marshalFullRecoveryStatusObject(t, jobs)
	data = marshalFullRecoveryStatusObject(t, status)
	snapshot, err := pueue.ParseQueueSnapshot(data, fixture.client.RuntimeBinding().ObservedVersion)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(snapshot.Jobs, func(job pueue.Job) bool { return job.ID == 8 })
	if index < 0 || snapshot.Jobs[index].State != pueue.StateRunning {
		t.Fatalf("running fault did not produce a canonical Running row: %+v", snapshot.Jobs)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeFullRecoveryStatusObject(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if object == nil {
		t.Fatal("status fixture is not a JSON object")
	}
	return object
}

func marshalFullRecoveryStatusObject(t *testing.T, object map[string]json.RawMessage) []byte {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireEventlessFullRecovery(t *testing.T, fixture pendingPublicationFixture) {
	t.Helper()
	for _, id := range []int64{7, 8} {
		_, found, err := fixture.client.ReadPrivateTerminalEvent(t.Context(), id)
		if err != nil || found {
			t.Fatalf("full recovery requires no candidate event for %d: found=%t err=%v", id, found, err)
		}
	}
	if fixture.launches.Load() != 0 {
		t.Fatal("fixture invoked a terminal callback before full recovery")
	}
}

func requireSafeFullRecoveryRemoval(t *testing.T, fixture pendingPublicationFixture, unsafeTicket string) {
	t.Helper()
	base := filepath.Join(fixture.store.Root, ".supervisor")
	data, err := os.ReadFile(filepath.Join(base, "terminal-event-removals"))
	if err != nil {
		t.Fatal(err)
	}
	if ids := strings.Fields(string(data)); !slices.Equal(ids, []string{"7"}) {
		t.Errorf("full recovery removed %v, want only safe row 7", ids)
	}
	data, err = os.ReadFile(filepath.Join(base, "terminal-event-tickets", "8.json"))
	if err != nil || string(data) != unsafeTicket {
		t.Errorf("unsafe row lost or changed its callback ticket: data=%s err=%v", data, err)
	}
	for _, path := range []string{
		filepath.Join(base, "terminal-event-tickets", "7.json"),
		filepath.Join(base, "terminal-events", "7.json"),
		filepath.Join(base, "terminal-events", "8.json"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("eventless recovery retained or created %s: %v", path, err)
		}
	}
}
