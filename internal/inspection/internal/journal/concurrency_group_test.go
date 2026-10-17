package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInspectionQueueReplayPreservesSavedBytesAndDeadline(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "legacy"
		if concurrent {
			name = "concurrent"
		}
		t.Run(name, func(t *testing.T) {
			testInspectionQueueReplayCase(t, concurrent)
		})
	}
}

func testInspectionQueueReplayCase(t *testing.T, concurrent bool) {
	t.Helper()
	store, request, binding := concurrentInspectionFixture(t)
	open, other := OpenOperation, OpenConcurrentOperation
	wantGroup := groupForRoot(store.RootID)
	if concurrent {
		open, other = other, open
		wantGroup = "default"
	}
	now := time.Unix(100, 123000000).UTC()
	first, err := open(store, request, binding, now)
	mustInspection(t, err)
	saved, digest, deadline := first.Request(), first.Digest(), first.Deadline()
	mustInspection(t, first.Close())
	if saved.Group != wantGroup || !deadline.Equal(now.Add(AdmissionTimeout)) {
		t.Fatalf("unexpected group or deadline: group=%q deadline=%v", saved.Group, deadline)
	}
	assertInspectionQueueIdentity(t, saved, wantGroup)
	mustInspection(t, ValidateRequestRecord(saved, store.RootID))
	path := filepath.Join(store.Root, "inspections", request.TaskID, requestRecordName)
	original, err := os.ReadFile(path)
	mustInspection(t, err)

	replayed, err := open(store, request, binding, now.Add(time.Hour))
	mustInspection(t, err)
	assertInspectionQueueReplay(t, replayed, saved, digest, deadline)
	if permit, claimErr := replayed.ClaimSubmission(deadline); permit != nil || !errors.Is(claimErr, ErrAdmissionExpired) {
		t.Fatalf("replay renewed submission authority: permit=%v err=%v", permit, claimErr)
	}
	mustInspection(t, replayed.Close())
	loaded, err := LoadOperationContext(t.Context(), store, request.TaskID)
	mustInspection(t, err)
	assertInspectionQueueReplay(t, loaded, saved, digest, deadline)
	mustInspection(t, loaded.Close())
	changed, err := other(store, request, binding, now.Add(2*time.Hour))
	if changed != nil {
		mustInspection(t, changed.Close())
	}
	if changed != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("wrapper changed saved queue: operation=%v err=%v", changed, err)
	}
	after, err := os.ReadFile(path)
	mustInspection(t, err)
	if !bytes.Equal(original, after) {
		t.Fatal("replay or load changed immutable request bytes")
	}
	if _, err = os.Stat(filepath.Join(store.Root, "inspection-group")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operation created a group journal: %v", err)
	}
}

func TestConcurrentInspectionRejectsIneligibleBindingBeforeCreationAndOnLoad(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      func(*Binding, string)
		recordValid bool
	}{
		{name: "legacy-revision", mutate: func(binding *Binding, _ string) { binding.DefinitionRevision = "inspection-v1" }},
		{name: "model-discovery", mutate: func(binding *Binding, _ string) { binding.DefinitionRevision = provider.ModelsRevision }},
		{name: "missing-capability", mutate: func(binding *Binding, _ string) { binding.CapabilitySHA256 = "" }},
		{name: "invalid-capability", mutate: func(binding *Binding, _ string) { binding.CapabilitySHA256 = "invalid" }},
		{name: "external-config", recordValid: true, mutate: func(binding *Binding, _ string) { binding.Supervisor.ConfigPath = "/external/pueue.yml" }},
		{name: "another-private-root", recordValid: true, mutate: func(binding *Binding, root string) {
			binding.Supervisor.ConfigPath = pueue.PrivateConfigPath(filepath.Join(root, "another-root"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, request, binding := concurrentInspectionFixture(t)
			invalid := binding
			tc.mutate(&invalid, store.Root)
			now := time.Unix(100, 0).UTC()
			op, err := OpenConcurrentOperation(store, request, invalid, now)
			if op != nil {
				mustInspection(t, op.Close())
			}
			if op != nil || err == nil {
				t.Fatalf("ineligible binding admitted: operation=%v err=%v", op, err)
			}
			dir := filepath.Join(store.Root, "inspections", request.TaskID)
			if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected binding created a journal: %v", err)
			}

			op, err = OpenConcurrentOperation(store, request, binding, now)
			mustInspection(t, err)
			saved := op.Request()
			mustInspection(t, op.Close())
			saved.Binding = invalid
			// Preserve a valid revision-specific deadline so rejection tests the
			// queue eligibility contract rather than an unrelated timing fault.
			saved.Deadline = now.Add(inspectionTimeout(invalid)).Format(time.RFC3339Nano)
			validationErr := ValidateRequestRecord(saved, store.RootID)
			if (validationErr == nil) != tc.recordValid {
				t.Fatalf("record validation eligibility=%v, want %v: %v", validationErr == nil, tc.recordValid, validationErr)
			}
			assertInspectionQueueLoadRejected(t, store, saved)
		})
	}
}

func TestInspectionQueueRejectsArbitrarySavedGroupsAndChangedLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*RequestRecord)
	}{
		{name: "empty", mutate: func(record *RequestRecord) { record.Group = "" }},
		{name: "arbitrary", mutate: func(record *RequestRecord) { record.Group = "unrelated" }},
		{name: "another-root", mutate: func(record *RequestRecord) { record.Group = groupForRoot(strings.Repeat("b", 32)) }},
		{name: "queue-derived-label", mutate: func(record *RequestRecord) { record.Label = "default-" + record.TaskID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, request, binding := concurrentInspectionFixture(t)
			op, err := OpenConcurrentOperation(store, request, binding, time.Unix(100, 0).UTC())
			mustInspection(t, err)
			saved := op.Request()
			mustInspection(t, op.Close())
			tc.mutate(&saved)
			if err = ValidateRequestRecord(saved, store.RootID); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("invalid group or label accepted by record validator: %v", err)
			}
			assertInspectionQueueLoadRejected(t, store, saved)
		})
	}
}

func TestInspectionQueuePermitsCaptureLoadedGroup(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "legacy"
		if concurrent {
			name = "concurrent"
		}
		t.Run(name, func(t *testing.T) {
			store, request, binding := concurrentInspectionFixture(t)
			open := OpenOperation
			wantGroup := groupForRoot(store.RootID)
			if concurrent {
				open, wantGroup = OpenConcurrentOperation, "default"
			}
			op, err := open(store, request, binding, time.Unix(100, 0).UTC())
			mustInspection(t, err)
			mustInspection(t, op.Close())
			loaded, err := LoadOperation(store, request.TaskID)
			mustInspection(t, err)
			t.Cleanup(func() { mustInspection(t, loaded.Close()) })
			submission, err := loaded.ClaimSubmission(time.Unix(101, 0).UTC())
			mustInspection(t, err)
			mustInspection(t, loaded.RecordReceipt(42))
			stop, stopRecord, err := loaded.ClaimStop(loaded.Deadline())
			mustInspection(t, err)
			assertInspectionPermitQueue(t, pueue.InspectionSubmissionPermit(submission), wantGroup)
			assertInspectionPermitQueue(t, pueue.InspectionStopPermit(stop), wantGroup)
			if stopRecord.Deadline != loaded.Request().Deadline || stopRecord.RequestSHA256 != loaded.Digest() {
				t.Fatal("stop permit lost the saved request or deadline binding")
			}
			assertInspectionQueueIdentity(t, loaded.Request(), wantGroup)
		})
	}
}

func TestInspectionQueueNilPermitAccessors(t *testing.T) {
	var submission *SubmissionPermit
	var stop *StopPermit
	if submission.InspectionQueueGroup() != "" || stop.InspectionQueueGroup() != "" {
		t.Fatal("nil permit supplied a queue group")
	}
}

func concurrentInspectionFixture(t *testing.T) (*taskdir.Store, task.TaskRecord, Binding) {
	t.Helper()
	store, request, binding := inspectionFixture(t)
	binding.DefinitionRevision = provider.RuntimeInspectionRevision
	binding.CapabilitySHA256 = task.ComputeSHA256([]byte("runtime-capability"))
	binding.Supervisor.ConfigPath = pueue.PrivateConfigPath(store.Root)
	return store, request, binding
}

func assertInspectionQueueIdentity(t *testing.T, record RequestRecord, wantGroup string) {
	t.Helper()
	identity := record.SupervisorIdentity()
	if identity.RootID != record.RootID || identity.TaskID != record.TaskID || identity.NumericTaskID != nil || identity.QueueGroup != wantGroup || identity.Group() != wantGroup {
		t.Fatalf("identity lost the saved queue or task: %+v", identity)
	}
	if identity.Label() != labelForIdentity(record.RootID, record.TaskID) || record.Label != identity.Label() {
		t.Fatalf("queue group changed the legacy label: %+v", identity)
	}
}

func assertInspectionQueueReplay(t *testing.T, op *Operation, saved RequestRecord, digest string, deadline time.Time) {
	t.Helper()
	if !reflect.DeepEqual(op.Request(), saved) || op.Digest() != digest || !op.Deadline().Equal(deadline) {
		t.Fatal("saved request, digest, or deadline changed on replay")
	}
}

func assertInspectionQueueLoadRejected(t *testing.T, store *taskdir.Store, record RequestRecord) {
	t.Helper()
	path := filepath.Join(store.Root, "inspections", record.TaskID, requestRecordName)
	data, err := task.MarshalCanonical(record)
	mustInspection(t, err)
	mustInspection(t, os.WriteFile(path, data, 0o600))
	loaded, err := LoadOperationContext(t.Context(), store, record.TaskID)
	if loaded != nil {
		mustInspection(t, loaded.Close())
	}
	if loaded != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("ineligible saved group loaded: operation=%v err=%v", loaded, err)
	}
	after, err := os.ReadFile(path)
	mustInspection(t, err)
	if !bytes.Equal(data, after) {
		t.Fatal("failed load changed persisted request bytes")
	}
}

func assertInspectionPermitQueue(t *testing.T, permit any, want string) {
	t.Helper()
	scoped, ok := permit.(interface{ InspectionQueueGroup() string })
	if !ok || scoped.InspectionQueueGroup() != want {
		t.Fatalf("permit %T lost its saved queue %q", permit, want)
	}
}
