package pueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecordPrivateTerminalEventSkipsOrdinaryTaskWithoutTakingEventLock(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	lockPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventLockName)
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := RecordPrivateTerminalEvent(context.Background(), fixture.stateRoot, 41, "0"); err != nil {
		t.Fatalf("ordinary task callback required the event lock: %v", err)
	}
	eventPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventRecordDir, "41.json")
	if _, err := os.Lstat(eventPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary task callback created terminal state: %v", err)
	}
}

func TestReadPrivateTerminalEventDoesNotProvisionAbsentEventLock(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	lockPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventLockName)
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, found, err := client.ReadPrivateTerminalEvent(context.Background(), 41); err != nil || found {
		t.Fatalf("read missing event found=%t err=%v", found, err)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read missing event provisioned callback lock: %v", err)
	}
}

func TestPrivateTerminalEventAPIsRejectNilContext(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	var nilContext context.Context

	if enabled, err := client.PrivateTerminalEventsEnabled(nilContext); !errors.Is(err, context.Canceled) || enabled {
		t.Fatalf("nil-context enabled returned enabled=%t err=%v, want context.Canceled", enabled, err)
	}
	if err := client.RegisterPrivateTerminalEvent(nilContext, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context register returned %v, want context.Canceled", err)
	}
	if err := client.RegisterPrivateOrdinaryTerminalEvent(nilContext, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context ordinary register returned %v, want context.Canceled", err)
	}
	if _, err := client.RecordPrivateTerminalObservation(nilContext, InspectionObservation{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context observation returned %v, want context.Canceled", err)
	}
	if pending, err := privateTerminalEventWakePending(nilContext, fixture.stateRoot); !errors.Is(err, context.Canceled) || pending {
		t.Fatalf("nil-context wake returned pending=%t err=%v, want context.Canceled", pending, err)
	}
	if event, found, err := client.ReadPrivateTerminalEvent(nilContext, 42); !errors.Is(err, context.Canceled) || found || event != (TerminalEvent{}) {
		t.Fatalf("nil-context read returned event=%+v found=%t err=%v, want context.Canceled", event, found, err)
	}
	if err := client.ClearPrivateTerminalEvents(nilContext, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context clear returned %v, want context.Canceled", err)
	}
	if err := RecordPrivateTerminalEvent(nilContext, fixture.stateRoot, 42, "0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context callback returned %v, want context.Canceled", err)
	}
}

func TestPrivateTerminalObservationPublishesMissingCallbackCandidate(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	id := int64(0)
	identity := InspectionIdentity{
		RootID:        strings.Repeat("a", 32),
		TaskID:        strings.Repeat("b", 32),
		NumericTaskID: &id,
		QueueGroup:    "default",
	}
	job := Job{ID: id, Label: stringPointer(identity.Label()), State: StateEnded, Group: identity.Group(), Succeeded: true}
	writeTerminalObservationStatus(t, fixture.stateRoot, job)
	observation := InspectionObservation{Binding: client.Binding(), Identity: identity, Job: job, Matched: true}
	if err := client.RegisterPrivateTerminalEvent(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if published, err := client.RecordPrivateTerminalObservation(t.Context(), observation); err != nil || !published {
		t.Fatalf("publish terminal observation: published=%t err=%v", published, err)
	}
	event, found, err := client.ReadPrivateTerminalEvent(t.Context(), id)
	if err != nil || !found || !event.Succeeded {
		t.Fatalf("application terminal observation event=%+v found=%t err=%v", event, found, err)
	}
	if published, err := client.RecordPrivateTerminalObservation(t.Context(), observation); err != nil || published {
		t.Fatalf("repeat terminal observation published=%t err=%v, want idempotent reuse", published, err)
	}
}

func TestPrivateTerminalObservationDoesNotCrossNumericIDReuse(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	id := int64(0)
	identity := InspectionIdentity{
		RootID:        strings.Repeat("a", 32),
		TaskID:        strings.Repeat("b", 32),
		NumericTaskID: &id,
		QueueGroup:    "default",
	}
	inspectionJob := Job{ID: id, Label: stringPointer(identity.Label()), State: StateEnded, Group: identity.Group(), Succeeded: true}
	observation := InspectionObservation{Binding: client.Binding(), Identity: identity, Job: inspectionJob, Matched: true}
	ordinary := statusTestJob(id, "delegate:"+strings.Repeat("c", 32)+":"+strings.Repeat("d", 32), statusStreamTestDoneState("Success"))
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	data := statusTestPayload(t, map[string]any{"0": ordinary}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if published, err := client.RecordPrivateTerminalObservation(t.Context(), observation); err != nil || published {
		t.Fatalf("stale terminal observation published=%t err=%v", published, err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(t.Context(), id); err != nil || found {
		t.Fatalf("stale observation recreated candidate after ID reuse: found=%t err=%v", found, err)
	}
}

func writeTerminalObservationStatus(t *testing.T, stateRoot string, job Job) {
	t.Helper()
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	row := statusTestJob(job.ID, *job.Label, statusStreamTestDoneState("Success"))
	data := statusTestPayload(t, map[string]any{"0": row}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func stringPointer(value string) *string { return &value }

func TestPrivateOrdinaryTerminalEventPublishesCallbackCandidate(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	ctx := t.Context()
	if err := client.RegisterPrivateOrdinaryTerminalEvent(ctx, 42); err != nil {
		t.Fatal(err)
	}
	recorded, err := RecordPrivateTerminalEventForMaintenance(ctx, fixture.stateRoot, 42, "0")
	if err != nil || !recorded {
		t.Fatalf("ordinary callback recorded=%t err=%v", recorded, err)
	}
	event, found, err := client.ReadPrivateTerminalEvent(ctx, 42)
	if err != nil || !found || event.NumericTaskID != 42 || !event.Succeeded {
		t.Fatalf("ordinary callback event=%+v found=%t err=%v", event, found, err)
	}
	pending, err := privateTerminalEventWakePending(ctx, fixture.stateRoot)
	if err != nil || !pending {
		t.Fatalf("ordinary callback wake pending=%t err=%v", pending, err)
	}
}

func TestPrivateOrdinaryTerminalEventRegistrationClearsStaleReusedIDEvent(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	ctx := t.Context()
	if err := client.RegisterPrivateOrdinaryTerminalEvent(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, fixture.stateRoot, 42, "0"); err != nil {
		t.Fatal(err)
	}

	if err := client.RegisterPrivateOrdinaryTerminalEvent(ctx, 42); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if present, err := readTerminalEventTicket(base, 42); err != nil || !present {
		t.Fatalf("reused ordinary ID ticket present=%t err=%v", present, err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(ctx, 42); err != nil || found {
		t.Fatalf("reused ordinary ID retained stale event: found=%t err=%v", found, err)
	}
	if err := RecordPrivateTerminalEvent(ctx, fixture.stateRoot, 42, "1"); err != nil {
		t.Fatal(err)
	}
	event, found, err := client.ReadPrivateTerminalEvent(ctx, 42)
	if err != nil || !found || event.Succeeded {
		t.Fatalf("reused ordinary ID event=%+v found=%t err=%v", event, found, err)
	}
}

func TestPrivateTerminalEventJobsFiltersEventOnlyByNumericID(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	ctx := t.Context()
	ordinaryMissing := "delegate:ordinary-missing"
	ordinaryEvent := "delegate:ordinary-event"
	inspectionEvent := inspectionGroupPrefix + "event"
	inspectionMissing := inspectionGroupPrefix + "missing"
	jobs := []Job{
		{ID: 10, Label: &ordinaryMissing, State: StateEnded},
		{ID: 11, Label: &ordinaryEvent, State: StateEnded},
		{ID: 12, Label: &inspectionEvent, State: StateEnded},
		{ID: 13, Label: &inspectionMissing, State: StateEnded},
	}
	if err := client.RegisterPrivateOrdinaryTerminalEvent(ctx, 11); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, fixture.stateRoot, 11, "0"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{12, 13} {
		if err := client.RegisterPrivateTerminalEvent(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordPrivateTerminalEvent(ctx, fixture.stateRoot, 12, "0"); err != nil {
		t.Fatal(err)
	}

	eventJobs, err := client.privateTerminalEventJobs(ctx, fixture.stateRoot, jobs, true)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalEventJobIDs(t, eventJobs, 11, 12)
	recoveryJobs, err := client.privateTerminalEventJobs(ctx, fixture.stateRoot, jobs, false)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalEventJobIDs(t, recoveryJobs, 10, 11, 12)
}

func TestPrivateTerminalEventJobsEventOnlyPreservesDuplicateLabels(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	ctx := t.Context()
	label := inspectionGroupPrefix + "duplicate"
	jobs := []Job{
		{ID: 20, Label: &label, State: StateEnded},
		{ID: 21, Label: &label, State: StateEnded},
	}
	if err := client.RegisterPrivateTerminalEvent(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, fixture.stateRoot, 20, "0"); err != nil {
		t.Fatal(err)
	}

	filtered, err := client.privateTerminalEventJobs(ctx, fixture.stateRoot, jobs, true)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalEventJobIDs(t, filtered, 20, 21)
	if filtered[0].Label == nil || filtered[1].Label == nil || *filtered[0].Label != label || *filtered[1].Label != label {
		t.Fatalf("duplicate labels were not preserved: %+v", filtered)
	}
}

func TestPrivateTerminalEventJobsEventOnlyPreservesUnlabeledAmbiguity(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	ctx := t.Context()
	labeled := "delegate:labeled"
	jobs := []Job{
		{ID: 30, State: StateEnded},
		{ID: 31, State: StateEnded},
		{ID: 32, Label: &labeled, State: StateEnded},
	}
	if err := client.RegisterPrivateTerminalEvent(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, fixture.stateRoot, 30, "0"); err != nil {
		t.Fatal(err)
	}

	filtered, err := client.privateTerminalEventJobs(ctx, fixture.stateRoot, jobs, true)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalEventJobIDs(t, filtered, 30, 31)
}

func TestPrivateTerminalEventWakePendingDoesNotProvisionEmptyInbox(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	lockPath := filepath.Join(base, privateTerminalEventLockName)
	eventDirectory := filepath.Join(base, privateTerminalEventRecordDir)
	if err := os.Remove(eventDirectory); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	pending, err := privateTerminalEventWakePending(t.Context(), fixture.stateRoot)
	if err != nil || pending {
		t.Fatalf("empty inbox pending=%t err=%v", pending, err)
	}
	for _, path := range []string{lockPath, eventDirectory} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty inbox read provisioned %s: %v", path, err)
		}
	}
}

func TestPrivateTerminalEventWakePendingRejectsMalformedInbox(t *testing.T) {
	t.Run("entry", func(t *testing.T) {
		fixture := newPrivateSupervisorTestFixture(t)
		client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
		if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 42); err != nil {
			t.Fatal(err)
		}
		if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 42, "0"); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventRecordDir)
		if err := os.WriteFile(filepath.Join(directory, "unexpected"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if pending, err := privateTerminalEventWakePending(t.Context(), fixture.stateRoot); !errors.Is(err, ErrBinding) || pending {
			t.Fatalf("malformed entry pending=%t err=%v, want binding error", pending, err)
		}
	})

	t.Run("record", func(t *testing.T) {
		fixture := newPrivateSupervisorTestFixture(t)
		client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
		if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 42); err != nil {
			t.Fatal(err)
		}
		if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 42, "0"); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventRecordDir, "42.json")
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if pending, err := privateTerminalEventWakePending(t.Context(), fixture.stateRoot); !errors.Is(err, ErrBinding) || pending {
			t.Fatalf("malformed record pending=%t err=%v, want binding error", pending, err)
		}
	})
}

func TestPrivateTerminalEventWakePendingRecoversInterruptedPublicationStage(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 42, "0"); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventRecordDir)
	eventPath := filepath.Join(directory, "42.json")
	stagePath := filepath.Join(directory, privateRecordStagePrefix+strings.Repeat("a", 32))
	if err := os.Link(eventPath, stagePath); err != nil {
		t.Fatal(err)
	}
	partialStagePath := filepath.Join(directory, privateRecordStagePrefix+strings.Repeat("b", 32))
	if err := os.WriteFile(partialStagePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	pending, err := privateTerminalEventWakePending(t.Context(), fixture.stateRoot)
	if err != nil || !pending {
		t.Fatalf("interrupted publication pending=%t err=%v", pending, err)
	}
	for _, path := range []string{stagePath, partialStagePath} {
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovered publication retained stage %s: %v", path, err)
		}
	}
	event, found, err := client.ReadPrivateTerminalEvent(t.Context(), 42)
	if err != nil || !found || event.NumericTaskID != 42 || !event.Succeeded {
		t.Fatalf("recovered publication event=%+v found=%t err=%v", event, found, err)
	}
}

func assertTerminalEventJobIDs(t *testing.T, jobs []Job, want ...int64) {
	t.Helper()
	if len(jobs) != len(want) {
		t.Fatalf("terminal event jobs=%+v, want IDs %v", jobs, want)
	}
	for index, id := range want {
		if jobs[index].ID != id {
			t.Fatalf("terminal event job IDs differ at %d: got %d want %d", index, jobs[index].ID, id)
		}
	}
}

func TestPrivateTerminalEventTicketPublishesIdempotentlyAndClearsAfterProof(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	ctx := context.Background()
	assertPrivateTerminalEventTicketPublishes(t, fixture.stateRoot, client, ctx)
	assertPrivateTerminalEventTicketClears(t, fixture.stateRoot, client, ctx)
}

func TestPrivateMaintenanceClearsStaleTerminalEventWhenRowIsAbsent(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	writePrivateTerminalEventStatus(t, fixture.stateRoot, nil)
	if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}
	runRequestedPrivateMaintenance(t, fixture.stateRoot, client, "private maintenance was unavailable")
	assertPrivateTerminalEventMetadataAbsent(t, fixture.stateRoot, client, 0)
}

func TestPrivateMaintenanceReconcilesStaleEventAfterMixedRemovalPass(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	writePrivateTerminalEventStatus(t, fixture.stateRoot, map[string]any{
		"0": statusTestJob(0, "finished", statusStreamTestDoneState("Success")),
	})
	for _, id := range []int64{0, 42} {
		if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, id, "0"); err != nil {
			t.Fatal(err)
		}
	}
	runRequestedPrivateMaintenance(t, fixture.stateRoot, client, "private maintenance was unavailable")
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("mixed maintenance removed unexpected numeric targets: %q", removed)
	}
	assertPrivateTerminalEventMetadataAbsent(t, fixture.stateRoot, client, 0)
	assertPrivateTerminalEventMetadataAbsent(t, fixture.stateRoot, client, 42)
}

func TestPrivateMaintenanceRecoversAfterTerminalEventClearFailure(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	writePrivateTerminalEventStatus(t, fixture.stateRoot, map[string]any{
		"0": statusTestJob(0, "finished", statusStreamTestDoneState("Success")),
	})
	if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	blockPrivateTerminalEventMetadata(t, fixture.stateRoot)
	runRequestedPrivateMaintenance(t, fixture.stateRoot, client, "private maintenance was unavailable")
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("successful remove command targeted %q, want 0", removed)
	}
	if pending, err := PrivateQueueMaintenanceRetryNeeded(fixture.stateRoot); err != nil || !pending {
		t.Fatalf("failed event clear pending=%t err=%v, want pending", pending, err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(t.Context(), 0); err != nil || !found {
		t.Fatalf("failed event clear found=%t err=%v, want retained event", found, err)
	}
	if present, err := readTerminalEventTicket(base, 0); err != nil || !present {
		t.Fatalf("failed event clear ticket present=%t err=%v, want retained ticket", present, err)
	}
	restorePrivateTerminalEventMetadata(t, fixture.stateRoot)
	writePrivateTerminalEventStatus(t, fixture.stateRoot, nil)
	runRequestedPrivateMaintenance(t, fixture.stateRoot, client, "private maintenance recovery was unavailable")
	assertPrivateTerminalEventMetadataAbsent(t, fixture.stateRoot, client, 0)
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("recovery removed unexpected numeric targets: %q", removed)
	}
}

func TestPrivateMaintenanceRetainsTerminalEventForReusedNumericID(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	var sawReusedRow bool
	client.options.SuccessfulCleanupAuthorizer = func(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		if len(jobs) == 1 && jobs[0].ID == 0 {
			sawReusedRow = true
		}
		return PrivateQueueCleanupPlan{}, nil
	}
	if err := client.RegisterPrivateOrdinaryTerminalEvent(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}
	newLabel := "delegate:reused:numeric-id"
	writePrivateTerminalEventStatus(t, fixture.stateRoot, map[string]any{"0": statusTestJob(0, newLabel, statusStreamTestDoneState("Success"))})
	if err := RequestPrivateCandidateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if !client.RunRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was unavailable")
	}
	if !sawReusedRow {
		t.Fatal("event-only maintenance did not expose the current reused row to the application authorizer")
	}
	event, found, err := client.ReadPrivateTerminalEvent(t.Context(), 0)
	if err != nil || !found || event.NumericTaskID != 0 {
		t.Fatalf("reused numeric ID event=%+v found=%t err=%v, want retained candidate", event, found, err)
	}
	if present, err := readTerminalEventTicket(filepath.Join(fixture.stateRoot, privateSupervisorDirectory), 0); err != nil || !present {
		t.Fatalf("reused numeric ID ticket present=%t err=%v, want retained ticket", present, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reused numeric ID was removed: %v", err)
	}
}

func writePrivateTerminalEventStatus(t *testing.T, stateRoot string, jobs map[string]any) {
	t.Helper()
	if jobs == nil {
		jobs = map[string]any{}
	}
	data := statusTestPayload(t, jobs, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	path := filepath.Join(stateRoot, privateSupervisorDirectory, "test-status.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runRequestedPrivateMaintenance(t *testing.T, stateRoot string, client *Client, unavailableMessage string) {
	t.Helper()
	if err := RequestPrivateCandidateQueueMaintenance(t.Context(), stateRoot); err != nil {
		t.Fatal(err)
	}
	if !client.RunRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal(unavailableMessage)
	}
}

func blockPrivateTerminalEventMetadata(t *testing.T, stateRoot string) {
	t.Helper()
	for _, directory := range privateTerminalEventMetadataDirectories(stateRoot) {
		if err := os.Chmod(directory, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { restorePrivateTerminalEventMetadata(t, stateRoot) })
}

func restorePrivateTerminalEventMetadata(t *testing.T, stateRoot string) {
	t.Helper()
	for _, directory := range privateTerminalEventMetadataDirectories(stateRoot) {
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Errorf("restore terminal event metadata directory permissions: %v", err)
		}
	}
}

func privateTerminalEventMetadataDirectories(stateRoot string) []string {
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	return []string{
		filepath.Join(base, privateTerminalEventRecordDir),
		filepath.Join(base, privateTerminalEventTicketDir),
	}
}

func assertPrivateTerminalEventMetadataAbsent(t *testing.T, stateRoot string, client *Client, id int64) {
	t.Helper()
	if _, found, err := client.ReadPrivateTerminalEvent(t.Context(), id); err != nil || found {
		t.Fatalf("terminal event found=%t err=%v, want absent", found, err)
	}
	if present, err := readTerminalEventTicket(filepath.Join(stateRoot, privateSupervisorDirectory), id); err != nil || present {
		t.Fatalf("terminal event ticket present=%t err=%v, want absent", present, err)
	}
	pending, err := PrivateQueueMaintenanceRetryNeeded(stateRoot)
	if err != nil || pending {
		t.Fatalf("maintenance retry pending=%t err=%v, want false", pending, err)
	}
}

func assertPrivateTerminalEventTicketPublishes(t *testing.T, stateRoot string, client *Client, ctx context.Context) {
	t.Helper()
	if enabled, err := client.PrivateTerminalEventsEnabled(ctx); err != nil || !enabled {
		t.Fatalf("terminal events enabled=%t err=%v", enabled, err)
	}
	if err := client.RegisterPrivateTerminalEvent(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, stateRoot, 42, "0"); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, stateRoot, 42, "0"); err != nil {
		t.Fatalf("same terminal callback was not idempotent: %v", err)
	}
	event, found, err := client.ReadPrivateTerminalEvent(ctx, 42)
	if err != nil || !found || !event.Succeeded || event.NumericTaskID != 42 {
		t.Fatalf("terminal event = %+v found=%t err=%v", event, found, err)
	}
	if err = RecordPrivateTerminalEvent(ctx, stateRoot, 42, "1"); !errors.Is(err, ErrBinding) {
		t.Fatalf("conflicting terminal callback error = %v, want binding error", err)
	}
}

func assertPrivateTerminalEventTicketClears(t *testing.T, stateRoot string, client *Client, ctx context.Context) {
	t.Helper()
	if err := client.ClearPrivateTerminalEvents(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(ctx, 42); err != nil || found {
		t.Fatalf("cleared terminal event found=%t err=%v", found, err)
	}
	if err := RecordPrivateTerminalEvent(ctx, stateRoot, 42, "0"); err != nil {
		t.Fatalf("callback after ticket clear failed: %v", err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(ctx, 42); err != nil || found {
		t.Fatalf("callback recreated cleared event: found=%t err=%v", found, err)
	}
}

func TestTerminalEventExitCodeIsStrict(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
		err  bool
	}{
		{code: "0", want: true},
		{code: "1"},
		{code: "-1"},
		{code: "None"},
		{code: "00", err: true},
		{code: "+1", err: true},
		{code: "1\n", err: true},
		{code: "", err: true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			got, err := terminalEventSucceeded(tc.code)
			if (err != nil) != tc.err || got != tc.want {
				t.Fatalf("terminalEventSucceeded(%q) = (%t, %v), want (%t, error=%t)", tc.code, got, err, tc.want, tc.err)
			}
		})
	}
}

func TestPrivateTerminalEventConfigRequiresExactCallback(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "state", privateSupervisorDirectory)
	config, err := ParseConfig([]byte(privateConfigYAML(base)))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveConfig(config, fixedResolution())
	if err != nil {
		t.Fatal(err)
	}
	if !privateTerminalEventsConfigured(base, resolved.Daemon.Callback) {
		t.Fatalf("generated private callback = %q, want exact event contract", valueOrEmpty(resolved.Daemon.Callback))
	}
	if err = resolved.ValidateIsolation(base); err != nil {
		t.Fatalf("generated private callback did not validate: %v", err)
	}
	callback := *resolved.Daemon.Callback + " unexpected"
	resolved.Daemon.Callback = &callback
	if err = resolved.ValidateIsolation(base); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unexpected callback validated: %v", err)
	}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
