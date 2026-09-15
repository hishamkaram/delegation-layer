package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func supervisorTask(t *testing.T, s *Store) *TaskDir {
	t.Helper()
	req, meta, brief := preparedInput(t, s)
	meta.SupervisorConfig.ClientExecutable = "/fake/pueue"
	meta.SupervisorConfig.ClientSHA256 = task.ComputeSHA256([]byte("client"))
	meta.SupervisorConfig.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("resolved"))
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	return td
}

func supervisorSubmit(t *testing.T, td *TaskDir) *task.SubmitRecord {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	must(t, err)
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	record, err := permit.Record()
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	return record
}

func supervisorReceipt(t *testing.T, td *TaskDir) task.SupervisorReceipt {
	t.Helper()
	submit, err := td.ReadSubmission()
	must(t, err)
	ref := submit.Supervisor
	r := task.SupervisorReceipt{SchemaVersion: task.SchemaVersion, RootID: submit.RootID, TaskID: submit.TaskID, SpecSHA256: submit.SpecSHA256, MetaSHA256: submit.MetaSHA256, NumericTaskID: 7, Label: submit.Label, ConfigPath: ref.ConfigPath, ConfigDigest: ref.ConfigDigest, Endpoint: ref.Endpoint, ObservedVersion: ref.ObservedVersion, ClientExecutable: ref.ClientExecutable, ClientSHA256: ref.ClientSHA256, ResolvedConfigSHA256: ref.ResolvedConfigSHA256}
	must(t, td.RecordSupervisorReceipt(r))
	return r
}

func TestBudgetStopRequiresBoundValidStartGuard(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*task.ProviderStartRecord)
		want   error
	}{
		{"valid", nil, nil},
		{"root", func(r *task.ProviderStartRecord) { r.RootID = strings.Repeat("e", 32) }, task.ErrIdentityMismatch},
		{"task", func(r *task.ProviderStartRecord) { r.TaskID = strings.Repeat("e", 32) }, task.ErrIdentityMismatch},
		{"spec", func(r *task.ProviderStartRecord) { r.SpecSHA256 = strings.Repeat("e", 64) }, task.ErrIdentityMismatch},
		{"meta", func(r *task.ProviderStartRecord) { r.MetaSHA256 = strings.Repeat("e", 64) }, task.ErrIdentityMismatch},
		{"budget", func(r *task.ProviderStartRecord) { r.BudgetNanos++ }, task.ErrIdentityMismatch},
		{"schema", func(r *task.ProviderStartRecord) { r.SchemaVersion++ }, task.ErrUnsupportedSchema},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td := supervisorTask(t, testStore(t))
			supervisorSubmit(t, td)
			supervisorReceipt(t, td)
			runner := consumeStart(t, td)
			guard, err := runner.Record()
			must(t, err)
			must(t, runner.Release())
			mutateStartGuardForTest(t, td, guard, tc.mutate)
			permit, err := td.PrepareStop("budget", "budget", time.Now().Add(time.Minute))
			if permit != nil {
				must(t, permit.Release())
			}
			if tc.want == nil {
				must(t, err)
				if permit == nil {
					t.Fatal("valid bound start did not grant budget request")
				}
				return
			}
			if permit != nil || !errors.Is(err, tc.want) {
				t.Fatalf("invalid start guard granted stop authority: permit=%v error=%v", permit != nil, err)
			}
			if _, err := td.ReadStopRequest("budget"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid guard persisted budget request: %v", err)
			}
		})
	}
}

func mutateStartGuardForTest(t *testing.T, td *TaskDir, guard *task.ProviderStartRecord, mutate func(*task.ProviderStartRecord)) {
	t.Helper()
	if mutate == nil {
		return
	}
	mutate(guard)
	data, err := task.MarshalCanonical(guard)
	must(t, err)
	writeTestFile(t, filepath.Join(td.Dir, "provider.start"), data)
}

func TestBriefExecutionDescriptorIsReadonlyAndExact(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	f, err := td.OpenBriefForExecution()
	must(t, err)
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	must(t, err)
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		t.Fatal("brief descriptor writable")
	}
	data, err := io.ReadAll(f)
	must(t, err)
	if !bytes.Equal(data, []byte("hello brief")) {
		t.Fatalf("brief bytes %q", data)
	}
	if _, err = f.Write([]byte("changed")); err == nil {
		t.Fatal("read-only brief accepted write")
	}
	must(t, f.Close())
	writeTestFile(t, filepath.Join(td.Dir, "brief.md"), []byte("changed"))
	if f, err = td.OpenBriefForExecution(); err == nil {
		must(t, f.Close())
		t.Fatal("changed brief exposed")
	}
}

func TestSubmissionRecordIsCopiedAndReleasedPermitInvalid(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	_, meta, err := td.PreparedRecords()
	must(t, err)
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	record, err := permit.Record()
	must(t, err)
	record.Supervisor.Endpoint = "changed"
	record.TaskID = "changed"
	again, err := permit.Record()
	must(t, err)
	if again.TaskID != td.TaskID || again.Supervisor.Endpoint != meta.SupervisorConfig.Endpoint {
		t.Fatal("record alias escaped")
	}
	copyPermit := *permit
	must(t, permit.Consume())
	if err = copyPermit.Consume(); !errors.Is(err, task.ErrPermitAlreadyUsed) {
		t.Fatal(err)
	}
	must(t, permit.Release())
	if _, err = copyPermit.Record(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
}

func TestSupervisorReceiptRejectsEveryBindingChange(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	r := supervisorReceipt(t, td)
	original := readTestFile(t, filepath.Join(td.Dir, "supervisor.ref.json"))
	changes := []func(*task.SupervisorReceipt){func(r *task.SupervisorReceipt) { r.TaskID = strings.Repeat("c", 32) }, func(r *task.SupervisorReceipt) { r.MetaSHA256 = task.ComputeSHA256(nil) }, func(r *task.SupervisorReceipt) { r.SpecSHA256 = task.ComputeSHA256(nil) }, func(r *task.SupervisorReceipt) { r.NumericTaskID++ }, func(r *task.SupervisorReceipt) { r.Label += "x" }, func(r *task.SupervisorReceipt) { r.ConfigPath += "x" }, func(r *task.SupervisorReceipt) { r.ConfigDigest = task.ComputeSHA256(nil) }, func(r *task.SupervisorReceipt) { r.Endpoint += "x" }, func(r *task.SupervisorReceipt) { r.ObservedVersion += "x" }, func(r *task.SupervisorReceipt) { r.ClientExecutable += "x" }, func(r *task.SupervisorReceipt) { r.ClientSHA256 = task.ComputeSHA256(nil) }, func(r *task.SupervisorReceipt) { r.ResolvedConfigSHA256 = task.ComputeSHA256(nil) }}
	for _, change := range changes {
		bad := r
		change(&bad)
		if err := td.RecordSupervisorReceipt(bad); err == nil {
			t.Fatalf("foreign receipt accepted: %+v", bad)
		}
	}
	must(t, td.RecordSupervisorReceipt(r))
	if !bytes.Equal(original, readTestFile(t, filepath.Join(td.Dir, "supervisor.ref.json"))) {
		t.Fatal("receipt replaced")
	}
}

func TestProviderIdentityRequiresLiveRunnerAndPrecedesEOF(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	identity := task.SessionIdentity{Provider: "fixture:test", ConversationID: "late-session"}
	if err := td.RecordProviderIdentity(identity); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
	permit := consumeStart(t, td)
	writer, err := td.OpenRawWriter("stdout")
	must(t, err)
	must(t, td.RecordProviderIdentity(identity))
	first := readTestFile(t, filepath.Join(td.Dir, "provider.ref.json"))
	fresh, err := s.OpenTask(td.TaskID)
	must(t, err)
	defer func() { must(t, fresh.Close()) }()
	got, err := fresh.ReadProviderIdentity()
	must(t, err)
	if got.ConversationID != identity.ConversationID {
		t.Fatal("late identity unavailable while raw open")
	}
	if err = fresh.RecordProviderIdentity(identity); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
	must(t, td.RecordProviderIdentity(identity))
	identity.ConversationID = "different"
	if err = td.RecordProviderIdentity(identity); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatal(err)
	}
	if !bytes.Equal(first, readTestFile(t, filepath.Join(td.Dir, "provider.ref.json"))) {
		t.Fatal("identity overwritten")
	}
	must(t, writer.Close())
	must(t, permit.Release())
	if err = td.RecordProviderIdentity(identity); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
}

func TestStopPermitDoesNotInterfereWithRunner(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	runner := consumeStart(t, td)
	writer, err := td.OpenRawWriter("stdout")
	must(t, err)
	fresh, err := s.OpenTask(td.TaskID)
	must(t, err)
	defer func() { must(t, fresh.Close()) }()
	id := strings.Repeat("c", 32)
	permit, err := fresh.PrepareStop(id, "user", time.Time{})
	must(t, err)
	copied := *permit
	request, err := permit.Request()
	must(t, err)
	request.TaskID = "changed"
	if request.NumericTaskID == nil {
		t.Fatal("saved supervisor target missing from stop request")
	}
	*request.NumericTaskID = 99
	original, err := copied.Request()
	must(t, err)
	if original.TaskID != td.TaskID || original.NumericTaskID == nil || *original.NumericTaskID != 7 {
		t.Fatal("mutable stop request")
	}
	must(t, permit.Consume())
	if err = copied.Consume(); !errors.Is(err, task.ErrPermitAlreadyUsed) {
		t.Fatal(err)
	}
	if p, retryErr := fresh.PrepareStop(id, "user", time.Time{}); p != nil || !errors.Is(retryErr, task.ErrStopAlreadyRequested) {
		t.Fatalf("duplicate stop authority: %v %v", p, retryErr)
	}
	must(t, permit.Release())
	if err = copied.Consume(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
	must(t, writer.Close())
	must(t, runner.Release())
	testAbsent(t, filepath.Join(td.Dir, "provider.exit"))
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
}

func TestStopTargetSnapshotRejectsChangedReceipt(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	id := strings.Repeat("f", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	request, err := permit.Request()
	must(t, err)
	if request.NumericTaskID == nil || *request.NumericTaskID != 7 {
		t.Fatalf("unexpected target snapshot: %+v", request.NumericTaskID)
	}
	must(t, permit.Release())

	changed := supervisorReceipt(t, td)
	changed.NumericTaskID = 8
	data, err := task.MarshalCanonical(changed)
	must(t, err)
	writeTestFile(t, filepath.Join(td.Dir, "supervisor.ref.json"), data)
	if _, err = td.ReadStopRequest(id); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed receipt target accepted: %v", err)
	}
	if err = td.RecordStopReply(id, task.StopReplyFacts{NumericTaskID: 8, Action: "kill", Acknowledged: true}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed receipt target reached reply: %v", err)
	}
}

func TestNilStopTargetIsNeverUpgradedByLaterReceipt(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	id := strings.Repeat("a", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	request, err := permit.Request()
	must(t, err)
	if request.NumericTaskID != nil {
		t.Fatalf("unexpected target snapshot: %v", *request.NumericTaskID)
	}
	must(t, permit.Consume())
	must(t, permit.Release())

	supervisorReceipt(t, td)
	if err = td.RecordStopReply(id, task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("nil snapshot upgraded by later receipt: %v", err)
	}
	testAbsent(t, filepath.Join(td.Dir, "stop", id+".reply.json"))

	newID := strings.Repeat("b", 32)
	newPermit, err := td.PrepareStop(newID, "user", time.Time{})
	must(t, err)
	newRequest, err := newPermit.Request()
	must(t, err)
	if newRequest.NumericTaskID == nil || *newRequest.NumericTaskID != 7 {
		t.Fatalf("new request did not snapshot later receipt: %+v", newRequest.NumericTaskID)
	}
	must(t, newPermit.Release())
}

func TestStopDurabilityFailureNeverGrantsAuthority(t *testing.T) {
	for _, fault := range []string{"write", "short-write", "file-barrier", "close", "link", "post-link", "after-barrier", "cleanup-destination"} {
		t.Run(fault, func(t *testing.T) {
			s := testStore(t)
			td := supervisorTask(t, s)
			supervisorSubmit(t, td)
			id := strings.Repeat("c", 32)
			s.SetFaultInjector(&orderedFault{target: id + ".request.json", fail: fault})
			permit, err := td.PrepareStop(id, "user", time.Time{})
			if permit != nil || err == nil {
				t.Fatalf("fault granted authority: %v %v", permit, err)
			}
			s.SetFaultInjector(nil)
			if fault == "post-link" || fault == "after-barrier" || fault == "cleanup-destination" {
				permit, err = td.PrepareStop(id, "user", time.Time{})
				if permit != nil || !errors.Is(err, task.ErrStopAlreadyRequested) {
					t.Fatalf("uncertain request replayed: %v %v", permit, err)
				}
			} else {
				testAbsent(t, filepath.Join(td.Dir, "stop", id+".request.json"))
			}
		})
	}
}

func TestStopReceiptsRemainDistinctAndBound(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	id := strings.Repeat("d", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	facts := task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true, Message: "accepted"}
	bad := facts
	bad.NumericTaskID = 8
	if err = td.RecordStopReply(id, bad); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatal(err)
	}
	must(t, td.RecordStopReply(id, facts))
	must(t, td.RecordStopReply(id, facts))
	testAbsent(t, filepath.Join(td.Dir, "stop", id+".observed.json"))
	if err = td.RecordStopObservation(id, task.StopObservationFacts{NumericTaskID: 7, State: "running"}); err == nil {
		t.Fatal("nonterminal observation froze receipt")
	}
	must(t, td.RecordStopObservation(id, task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}))
	observation, err := td.ReadStopObservation(id)
	must(t, err)
	if !observation.Terminated {
		t.Fatal("end observation missing")
	}
	testAbsent(t, filepath.Join(td.Dir, "provider.exit"))
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
}

func TestContextStopRecordsRejectCanceledBeforeStaging(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	id := strings.Repeat("7", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	must(t, permit.Release())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	facts := task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true, Message: "accepted"}
	if err = td.RecordStopReplyContext(ctx, id, facts); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stop reply was staged: %v", err)
	}
	if err = td.RecordStopObservationContext(ctx, id, task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stop observation was staged: %v", err)
	}
	testAbsent(t, filepath.Join(td.Dir, "stop", id+".reply.json"))
	testAbsent(t, filepath.Join(td.Dir, "stop", id+".observed.json"))
}

func TestContextStopRecordsReplayAndConflictPreserveBytes(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	id := strings.Repeat("8", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	must(t, permit.Release())
	ctx := context.Background()
	facts := task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true, Message: "accepted"}
	must(t, td.RecordStopReplyContext(ctx, id, facts))
	replyBytes := readTestFile(t, filepath.Join(td.Dir, "stop", id+".reply.json"))
	must(t, td.RecordStopReplyContext(ctx, id, facts))
	if !bytes.Equal(replyBytes, readTestFile(t, filepath.Join(td.Dir, "stop", id+".reply.json"))) {
		t.Fatal("identical stop reply replay replaced authoritative bytes")
	}
	conflict := facts
	conflict.Acknowledged = false
	if err = td.RecordStopReplyContext(ctx, id, conflict); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting stop reply was accepted: %v", err)
	}
	if !bytes.Equal(replyBytes, readTestFile(t, filepath.Join(td.Dir, "stop", id+".reply.json"))) {
		t.Fatal("conflicting stop reply mutated authoritative bytes")
	}

	observationFacts := task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}
	must(t, td.RecordStopObservationContext(ctx, id, observationFacts))
	observationPath := filepath.Join(td.Dir, "stop", id+".observed.json")
	observationBytes := readTestFile(t, observationPath)
	must(t, td.RecordStopObservationContext(ctx, id, observationFacts))
	if !bytes.Equal(observationBytes, readTestFile(t, observationPath)) {
		t.Fatal("identical stop observation replay replaced authoritative bytes")
	}
}

func TestReadStopRecordsPreservesUnknownOptionalFacts(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	firstID, secondID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	firstPermit, err := td.PrepareStop(firstID, "user", time.Time{})
	must(t, err)
	must(t, firstPermit.Release())
	secondPermit, err := td.PrepareStop(secondID, "user", time.Time{})
	must(t, err)
	must(t, secondPermit.Release())
	must(t, td.RecordStopReply(firstID, task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true, Message: "accepted"}))
	must(t, td.RecordStopObservation(firstID, task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}))

	rows, err := td.ReadStopRecords()
	must(t, err)
	if len(rows) != 2 || rows[0].Request.RequestID != firstID || rows[1].Request.RequestID != secondID {
		t.Fatalf("stop records not sorted or complete: %+v", rows)
	}
	if rows[0].Reply == nil || rows[0].Observation == nil || rows[1].Reply != nil || rows[1].Observation != nil {
		t.Fatalf("optional stop facts changed: %+v", rows)
	}
	rows[0].Request.TaskID = "changed"
	again, err := td.ReadStopRecords()
	must(t, err)
	if again[0].Request.TaskID != td.TaskID {
		t.Fatal("stop record read escaped mutable storage")
	}

	writeTestFile(t, filepath.Join(td.Dir, "stop", secondID+".reply.json"), []byte("malformed"))
	if _, err = td.ReadStopRecords(); err == nil {
		t.Fatal("malformed reply was treated as absent")
	}
}

func TestReadStopRecordsRejectsOrphanedSideRecord(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	must(t, td.store.mkdir(filepath.Join(td.Dir, "stop"), nil))
	writeTestFile(t, filepath.Join(td.Dir, "stop", "orphan.reply.json"), []byte("{}"))
	if _, err := td.ReadStopRecords(); err == nil {
		t.Fatal("orphaned reply was treated as absent")
	}
}

func TestReadStopRecordsWithoutStopDirectoryIsEmpty(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	rows, err := td.ReadStopRecords()
	must(t, err)
	if rows == nil || len(rows) != 0 {
		t.Fatalf("unexpected records: %+v", rows)
	}
}

func TestStopInspectionRecognizesOnlyDurableStageNames(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	stopDir := filepath.Join(td.Dir, "stop")
	must(t, td.store.mkdir(stopDir, nil))
	validStage := filepath.Join(stopDir, "stage."+strings.Repeat("a", 32)+".tmp")
	writeTestFile(t, validStage, []byte("partial"))
	rows, err := td.ReadStopRecords()
	must(t, err)
	if len(rows) != 0 {
		t.Fatalf("staged stop record became durable: %+v", rows)
	}
	writeTestFile(t, filepath.Join(stopDir, "stage.not-a-real-id.tmp"), []byte("partial"))
	if _, err = td.ReadStopRecords(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("malformed stage name was ignored: %v", err)
	}
}

func TestStopInspectionBoundsEntriesAndEncodedBytes(t *testing.T) {
	ids := newStopRecordIDs()
	for i := 0; i < MaxStopRecords; i++ {
		id := fmt.Sprintf("%032x", i)
		if err := addStopRecordName(&ids, id+stopRequestSuffix); err != nil {
			t.Fatalf("request %d rejected before cap: %v", i, err)
		}
	}
	if err := addStopRecordName(&ids, fmt.Sprintf("%032x", MaxStopRecords)+stopRequestSuffix); !errors.Is(err, ErrStopInspectionLimit) {
		t.Fatalf("request cap not enforced: %v", err)
	}

	budget := stopInspectionBudget{}
	data := make([]byte, task.MaxControlRecordSize)
	for i := 0; i < MaxStopInspectionBytes/task.MaxControlRecordSize; i++ {
		if err := budget.reserve(data); err != nil {
			t.Fatalf("encoded-byte budget rejected boundary record %d: %v", i, err)
		}
	}
	if err := budget.reserve(data); !errors.Is(err, ErrStopInspectionLimit) {
		t.Fatalf("encoded-byte budget not enforced: %v", err)
	}
}

func exactEscapedControlValue(t *testing.T, build func(string) any) string {
	t.Helper()
	base, err := task.MarshalCanonical(build(""))
	must(t, err)
	remaining := task.MaxControlRecordSize - len(base)
	if remaining < 2 {
		t.Fatal("control record fixture has no room for a value")
	}
	value := strings.Repeat("\"", remaining/2)
	if remaining%2 != 0 {
		value += "x"
	}
	data, err := task.MarshalCanonical(build(value))
	must(t, err)
	if len(data) != task.MaxControlRecordSize {
		t.Fatalf("escaped control value did not reach limit: %d", len(data))
	}
	return value
}

func TestProviderIdentityControlLimitAndBoundaryRetry(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	permit := consumeStart(t, td)
	writer, err := td.OpenRawWriter("stdout")
	must(t, err)
	t.Cleanup(func() {
		must(t, writer.Close())
		must(t, permit.Release())
	})
	tooLarge := task.SessionIdentity{Provider: "fixture:test", ConversationID: strings.Repeat("\"", task.MaxControlRecordSize)}
	if err = td.RecordProviderIdentity(tooLarge); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized provider identity accepted: %v", err)
	}
	testAbsent(t, filepath.Join(td.Dir, "provider.ref.json"))

	_, req, _, spec, meta, err := td.loadAndValidatePreparedSet()
	must(t, err)
	observedAt := "2026-09-14T00:00:00.123456789Z"
	value := exactEscapedControlValue(t, func(conversation string) any {
		return task.ProviderRefRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: meta, Provider: req.Provider, ConversationID: conversation, ObservedAt: observedAt}
	})
	identity := task.SessionIdentity{Provider: req.Provider, ConversationID: value}
	record := task.ProviderRefRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: meta, Provider: req.Provider, ConversationID: value, ObservedAt: observedAt}
	data, err := task.MarshalCanonical(record)
	must(t, err)
	_, cleanup, err := td.store.stageAndCommit(td.Dir, "provider.ref.json", data, nil)
	must(t, errors.Join(err, cleanup))
	original := readTestFile(t, filepath.Join(td.Dir, "provider.ref.json"))
	if len(original) != task.MaxControlRecordSize {
		t.Fatalf("provider identity boundary size changed: %d", len(original))
	}
	must(t, td.RecordProviderIdentity(identity))
	if !bytes.Equal(original, readTestFile(t, filepath.Join(td.Dir, "provider.ref.json"))) {
		t.Fatal("boundary provider identity retry replaced bytes")
	}
}

func TestStopReplyControlLimitAndBoundaryRetry(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	id := strings.Repeat("3", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	must(t, permit.Release())
	tooLarge := task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true, Message: strings.Repeat("\"", task.MaxControlRecordSize)}
	if err = td.RecordStopReply(id, tooLarge); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized stop reply accepted: %v", err)
	}
	testAbsent(t, filepath.Join(td.Dir, "stop", id+".reply.json"))

	request, digest, receipt, err := td.stopEvidence(id)
	must(t, err)
	ref := request.Supervisor
	base := task.StopReplyRecord{SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID, SpecSHA256: request.SpecSHA256, MetaSHA256: request.MetaSHA256, Label: request.Label, Supervisor: &ref, RequestID: id, RequestSHA256: digest, NumericTaskID: &receipt.NumericTaskID, Action: "kill", Acknowledged: true, RepliedAt: "2026-09-14T00:00:00.123456789Z"}
	message := exactEscapedControlValue(t, func(value string) any {
		candidate := base
		candidate.Message = value
		return candidate
	})
	facts := task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true, Message: message}
	base.Message = message
	data, err := task.MarshalCanonical(base)
	must(t, err)
	_, cleanup, err := td.store.stageAndCommit(filepath.Join(td.Dir, "stop"), id+".reply.json", data, nil)
	must(t, errors.Join(err, cleanup))
	original := readTestFile(t, filepath.Join(td.Dir, "stop", id+".reply.json"))
	if len(original) != task.MaxControlRecordSize {
		t.Fatalf("stop reply boundary size changed: %d", len(original))
	}
	must(t, td.RecordStopReply(id, facts))
	if !bytes.Equal(original, readTestFile(t, filepath.Join(td.Dir, "stop", id+".reply.json"))) {
		t.Fatal("boundary stop reply retry replaced bytes")
	}
}

func TestBudgetRequestIdentityAndDeadline(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	if p, err := td.PrepareStop("budget", "budget", time.Now()); p != nil || err == nil {
		t.Fatal("queued budget request accepted")
	}
	runner := consumeStart(t, td)
	defer func() { must(t, runner.Release()) }()
	if p, err := td.PrepareStop("budget", "budget", time.Time{}); p != nil || err == nil {
		t.Fatal("missing deadline accepted")
	}
	deadline := time.Now().Add(time.Minute)
	p, err := td.PrepareStop("budget", "budget", deadline)
	must(t, err)
	request, err := p.Request()
	must(t, err)
	if request.Deadline != deadline.UTC().Format(time.RFC3339Nano) || request.BudgetNanos != int64(time.Minute) {
		t.Fatal("budget evidence changed")
	}
	must(t, p.Release())
	if err = p.Consume(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
}

func TestStopTerminalNoopAndLateReceiptRead(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	runner := consumeStart(t, td)
	must(t, td.WriteRawFiles("answer", ""))
	_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, runner.Release())
	collectOK(t, td)
	if p, err := td.PrepareStop(strings.Repeat("e", 32), "user", time.Time{}); p != nil || !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("terminal stop authority: %v %v", p, err)
	}
	if err := os.RemoveAll(filepath.Join(td.Dir, "stop")); err != nil {
		t.Fatal(err)
	}
}
