package inspection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestOpenOperationCreatesImmutableRequestOutsideOrdinaryTasks(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 123456789, time.FixedZone("test", 7*60*60))
	op, err := OpenOperation(store, request, binding, now)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })

	saved := op.Request()
	identity := struct{ root, task, group, label string }{store.RootID, request.TaskID, groupForRoot(store.RootID), labelForIdentity(store.RootID, request.TaskID)}
	gotIdentity := struct{ root, task, group, label string }{saved.RootID, saved.TaskID, saved.Group, saved.Label}
	if gotIdentity != identity {
		t.Fatalf("request identity mismatch: %+v", saved)
	}
	if saved.CreatedAt != now.UTC().Format(time.RFC3339Nano) || saved.Deadline != now.UTC().Add(AdmissionTimeout).Format(time.RFC3339Nano) {
		t.Fatalf("request deadline was not canonical: created=%q deadline=%q", saved.CreatedAt, saved.Deadline)
	}
	if op.Digest() == "" || op.Digest() != requestDigest(saved) {
		t.Fatalf("request digest mismatch: %q", op.Digest())
	}
	if _, openErr := store.OpenTask(request.TaskID); !errors.Is(openErr, os.ErrNotExist) {
		t.Fatalf("inspection created an ordinary task: %v", openErr)
	}

	control, err := store.OpenInspection(request.TaskID, false)
	mustInspection(t, err)
	defer func() { mustInspection(t, control.Close()) }()
	var onDisk RequestRecord
	mustInspection(t, control.Read(requestRecordName, &onDisk))
	if !reflect.DeepEqual(onDisk, saved) {
		t.Fatalf("persisted request differs: %+v / %+v", onDisk, saved)
	}
	// Request returns an owned copy, including the nested optional session.
	mutated := op.Request()
	mutated.Task.Provider = "fixture:mutated"
	if mutated.Task.PriorSession != nil {
		mutated.Task.PriorSession.ConversationID = "mutated"
	}
	if got := op.Request(); got.Task.Provider != saved.Task.Provider || !reflect.DeepEqual(got.Task.PriorSession, saved.Task.PriorSession) {
		t.Fatalf("request copy mutation changed journal state: got=%+v saved=%+v", got, saved)
	}
}

func TestLoadOperationContextHonorsCanceledCaller(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
	mustInspection(t, err)
	mustInspection(t, op.Close())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if loaded, loadErr := LoadOperationContext(ctx, store, request.TaskID); loaded != nil || !errors.Is(loadErr, context.Canceled) {
		t.Fatalf("canceled load crossed journal boundary: operation=%v err=%v", loaded, loadErr)
	}
}

func TestOpenOperationReplayRetainsOriginalDeadline(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	first, err := OpenOperation(store, request, binding, time.Unix(100, 222000000).UTC())
	mustInspection(t, err)
	firstRequest := first.Request()
	firstDigest := first.Digest()
	mustInspection(t, first.Close())

	second, err := OpenOperation(store, request, binding, time.Unix(1000, 999000000).UTC())
	mustInspection(t, err)
	defer func() { mustInspection(t, second.Close()) }()
	if second.Request().CreatedAt != firstRequest.CreatedAt || second.Request().Deadline != firstRequest.Deadline || second.Digest() != firstDigest {
		t.Fatalf("replay changed immutable timing or digest: first=%+v second=%+v", firstRequest, second.Request())
	}
}

func TestOpenOperationRejectsSemanticallyEqualNonCanonicalRequest(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	first, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
	mustInspection(t, err)
	mustInspection(t, first.Close())

	path := filepath.Join(store.Root, "inspections", request.TaskID, requestRecordName)
	canonical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The decoded request remains semantically identical, but this is not the
	// canonical byte representation that the create-once winner published.
	if err = os.WriteFile(path, append([]byte(" \n"), canonical...), 0o600); err != nil {
		t.Fatal(err)
	}
	if replay, replayErr := OpenOperation(store, request, binding, time.Unix(101, 0).UTC()); replay != nil || !errors.Is(replayErr, task.ErrEvidenceFault) {
		t.Fatalf("noncanonical request was accepted: operation=%v err=%v", replay, replayErr)
	}
}

func TestOpenOperationExpiredReplayRefusesClaims(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	createdAt := time.Unix(100, 0).UTC()
	first, err := OpenOperation(store, request, binding, createdAt)
	mustInspection(t, err)
	mustInspection(t, first.Close())

	later := createdAt.Add(AdmissionTimeout)
	op, err := OpenOperation(store, request, binding, later)
	mustInspection(t, err)
	defer func() { mustInspection(t, op.Close()) }()
	if !op.Expired(later) {
		t.Fatal("expired operation reported as live")
	}
	permit, err := op.ClaimSubmission(later)
	if permit != nil || !errors.Is(err, ErrAdmissionExpired) {
		t.Fatalf("expired operation yielded submission authority: permit=%v err=%v", permit, err)
	}
}

func TestOpenOperationRejectsChangedRequestAndBinding(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
	mustInspection(t, err)
	mustInspection(t, op.Close())

	changedRequest := request
	changedRequest.RequestedConfig.Model = "changed"
	if _, err = OpenOperation(store, changedRequest, binding, time.Unix(200, 0).UTC()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed request was accepted: %v", err)
	}
	changedBinding := binding
	changedBinding.HelperSHA256 = strings.Repeat("b", 64)
	if _, err = OpenOperation(store, request, changedBinding, time.Unix(200, 0).UTC()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed binding was accepted: %v", err)
	}
}

func TestInspectionClaimsHaveOneWinnerAndDistinctPermits(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
	mustInspection(t, err)
	defer func() { mustInspection(t, op.Close()) }()

	const callers = 8
	type claimResult struct {
		permit *SubmissionPermit
		err    error
	}
	results := make(chan claimResult, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			permit, claimErr := op.ClaimSubmission(time.Unix(101, 0).UTC())
			results <- claimResult{permit: permit, err: claimErr}
		})
	}
	wg.Wait()
	close(results)
	var winner *SubmissionPermit
	winners := 0
	for result := range results {
		if result.permit != nil {
			winner = result.permit
			winners++
			continue
		}
		if result.err == nil || !errors.Is(result.err, task.ErrAlreadySubmitted) {
			t.Fatalf("duplicate submission claim error=%v", result.err)
		}
	}
	if winners != 1 || winner == nil {
		t.Fatalf("submission claims=%d, want one permit", winners)
	}
	if winner.InspectionSubmissionRootID() != request.RootID || winner.InspectionSubmissionTaskID() != request.TaskID || winner.InspectionSubmissionSupervisor() != binding.Supervisor {
		t.Fatalf("submission permit lost immutable request binding: root=%q task=%q supervisor=%+v", winner.InspectionSubmissionRootID(), winner.InspectionSubmissionTaskID(), winner.InspectionSubmissionSupervisor())
	}
	mustInspection(t, winner.Consume())
	if !errors.Is(winner.Consume(), task.ErrPermitAlreadyUsed) {
		t.Fatal("submission permit was reusable")
	}

	start, err := op.ClaimStart(time.Unix(102, 0).UTC())
	mustInspection(t, err)
	if start == nil {
		t.Fatal("missing distinct start permit")
	}
	mustInspection(t, start.Consume())
	if !errors.Is(start.Consume(), task.ErrPermitAlreadyUsed) {
		t.Fatal("start permit was reusable")
	}
}

func TestInspectionClaimReplayRetainsOriginalGuardAtDifferentTime(t *testing.T) {
	op := evidenceOperation(t)
	first, err := op.ClaimSubmission(time.Unix(101, 0).UTC())
	mustInspection(t, err)
	if first == nil {
		t.Fatal("missing initial submission permit")
	}
	var before SubmissionRecord
	mustInspection(t, op.control.Read(submissionRecordName, &before))
	replay, err := op.ClaimSubmission(time.Unix(102, 0).UTC())
	if replay != nil || !errors.Is(err, task.ErrAlreadySubmitted) {
		t.Fatalf("submission replay at a different time: permit=%v err=%v", replay, err)
	}
	var after SubmissionRecord
	mustInspection(t, op.control.Read(submissionRecordName, &after))
	if before != after {
		t.Fatalf("submission replay changed guard: before=%+v after=%+v", before, after)
	}

	start, err := op.ClaimStart(time.Unix(103, 0).UTC())
	mustInspection(t, err)
	if start == nil {
		t.Fatal("missing initial start permit")
	}
	var startBefore StartRecord
	mustInspection(t, op.control.Read(startRecordName, &startBefore))
	startReplay, err := op.ClaimStart(time.Unix(104, 0).UTC())
	if startReplay != nil || !errors.Is(err, task.ErrAlreadyStarted) {
		t.Fatalf("start replay at a different time: permit=%v err=%v", startReplay, err)
	}
	var startAfter StartRecord
	mustInspection(t, op.control.Read(startRecordName, &startAfter))
	if startBefore != startAfter {
		t.Fatalf("start replay changed guard: before=%+v after=%+v", startBefore, startAfter)
	}
}

func TestInspectionClaimReplayRejectsMalformedDigestAndOutOfRangeGuards(t *testing.T) {
	tests := []struct {
		name   string
		record any
		raw    []byte
	}{
		{name: "malformed", raw: []byte("not-json")},
		{name: "wrong digest", record: SubmissionRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: strings.Repeat("b", 64), CreatedAt: time.Unix(101, 0).UTC().Format(time.RFC3339Nano)}},
		{name: "before operation", record: SubmissionRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: strings.Repeat("a", 64), CreatedAt: time.Unix(99, 0).UTC().Format(time.RFC3339Nano)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, request, binding := inspectionFixture(t)
			op, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
			mustInspection(t, err)
			t.Cleanup(func() { mustInspection(t, op.Close()) })
			path := filepath.Join(store.Root, "inspections", request.TaskID, submissionRecordName)
			if test.raw != nil {
				if writeErr := os.WriteFile(path, test.raw, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			} else {
				if _, putErr := op.control.Put(submissionRecordName, test.record); putErr != nil {
					t.Fatal(putErr)
				}
			}
			permit, err := op.ClaimSubmission(time.Unix(102, 0).UTC())
			if permit != nil || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("invalid existing guard accepted: permit=%v err=%v", permit, err)
			}
		})
	}
}

func TestInspectionClaimUncertainPublicationYieldsNoPermit(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
	mustInspection(t, err)
	defer func() { mustInspection(t, op.Close()) }()
	fault := &inspectionPostLinkFault{}
	store.SetFaultInjector(fault)
	permit, err := op.ClaimSubmission(time.Unix(101, 0).UTC())
	if permit != nil || !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("uncertain claim yielded authority: permit=%v err=%v", permit, err)
	}
	store.SetFaultInjector(nil)
	permit, err = op.ClaimSubmission(time.Unix(101, 0).UTC())
	if permit != nil || !errors.Is(err, task.ErrAlreadySubmitted) {
		t.Fatalf("uncertain winner was not retained as consumed claim: permit=%v err=%v", permit, err)
	}
}

func TestInspectionClosedOperationYieldsNoAuthority(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
	mustInspection(t, err)
	mustInspection(t, op.Close())
	permit, err := op.ClaimSubmission(time.Unix(101, 0).UTC())
	if permit != nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed operation yielded authority: permit=%v err=%v", permit, err)
	}
	if err = op.Close(); err != nil {
		t.Fatalf("second close failed: %v", err)
	}
}

func TestOpenOperationRejectsMalformedExistingRequestRecords(t *testing.T) {
	cases := map[string][]byte{
		"missing required field": []byte(`{"schema_version":1,"root_id":"11111111111111111111111111111111"}`),
		"unknown field":          []byte(`{"schema_version":1,"root_id":"11111111111111111111111111111111","task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","unknown":1}`),
		"null root":              []byte(`{"schema_version":1,"root_id":null,"task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			store, request, binding := inspectionFixture(t)
			op, err := OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
			mustInspection(t, err)
			mustInspection(t, op.Close())
			path := filepath.Join(store.Root, "inspections", request.TaskID, requestRecordName)
			if err = os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if replay, replayErr := OpenOperation(store, request, binding, time.Unix(101, 0).UTC()); replay != nil || replayErr == nil {
				t.Fatalf("malformed request was accepted: operation=%v err=%v", replay, replayErr)
			}
		})
	}
}

type inspectionPostLinkFault struct{ taskdir.BaseFaultInjector }

func (f *inspectionPostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == submissionRecordName {
		return errors.New("injected inspection post-link failure")
	}
	return nil
}

func inspectionFixture(t *testing.T) (*taskdir.Store, task.TaskRecord, Binding) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, store.Close()) })
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	mustInspection(t, err)
	taskID := strings.Repeat("a", 32)
	brief := []byte("inspection brief")
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		TaskID:        taskID,
		Provider:      "fixture:test",
		Mode:          "read-only",
		CanonicalCwd:  workspace,
		RequestedConfig: task.TaskConfig{
			Permission: "read-only",
			Budget:     "1m0s",
		},
		BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	digest := strings.Repeat("a", 64)
	binding := Binding{
		DefinitionRevision: "inspection-v1",
		DefinitionSHA256:   digest,
		HelperExecutable:   "/usr/local/bin/inspection-helper",
		HelperSHA256:       digest,
		WorkerExecutable:   "/usr/local/bin/inspection-worker",
		WorkerSHA256:       digest,
		Supervisor: task.SupervisorRef{
			ClientExecutable:     "/usr/local/bin/pueue",
			ClientSHA256:         digest,
			ResolvedConfigSHA256: digest,
			Endpoint:             "/private/pueue.sock",
			ConfigPath:           "/private/pueue.yml",
			ConfigDigest:         digest,
			ObservedVersion:      pueue.SupportedVersion,
		},
	}
	return store, request, binding
}

func mustInspection(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
