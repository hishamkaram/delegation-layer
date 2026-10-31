package supervisor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// scriptedInspectionEvidence accepts every lifetime and answers custody by
// task ID, recording each custody call.
type scriptedInspectionEvidence struct {
	invalid map[string]bool
	// releaseOnPrepare makes a fault-free PrepareVerifiedLifetime release the
	// custody that AuthorizeInspectionRemoval later reports.
	releaseOnPrepare bool

	mu            sync.Mutex
	released      map[string]bool
	removalFaults map[string]error
	prepareFaults map[string]error
	calls         []string
}

func (e *scriptedInspectionEvidence) ValidateLifetime(_ context.Context, operation *inspection.Operation, _ string, _ pueue.Job) error {
	if e.invalid[operation.Request().TaskID] {
		return inspection.ErrGroupMigrationRequired
	}
	return nil
}

func (*scriptedInspectionEvidence) RecoverLifetime(context.Context, *inspection.Operation) error {
	return nil
}

func (*scriptedInspectionEvidence) RecordedTerminal(context.Context, *inspection.Operation) (bool, error) {
	return true, nil
}

func (e *scriptedInspectionEvidence) PrepareVerifiedLifetime(_ context.Context, operation *inspection.Operation) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := operation.Request().TaskID
	e.calls = append(e.calls, "prepare:"+id)
	fault := e.prepareFaults[id]
	if e.releaseOnPrepare && fault == nil {
		if e.released == nil {
			e.released = make(map[string]bool)
		}
		e.released[id] = true
	}
	return fault
}

func (e *scriptedInspectionEvidence) AuthorizeInspectionRemoval(_ context.Context, operation *inspection.Operation) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := operation.Request().TaskID
	e.calls = append(e.calls, "authorize:"+id)
	return e.released[id], e.removalFaults[id]
}

func (e *scriptedInspectionEvidence) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	calls := slices.Clone(e.calls)
	slices.Sort(calls)
	return calls
}

// nativeOnlyEvidence is the same evidence without the custody extension.
type nativeOnlyEvidence struct{ evidence *scriptedInspectionEvidence }

func (n nativeOnlyEvidence) ValidateLifetime(ctx context.Context, operation *inspection.Operation, id string, job pueue.Job) error {
	return n.evidence.ValidateLifetime(ctx, operation, id, job)
}

func (n nativeOnlyEvidence) RecoverLifetime(ctx context.Context, operation *inspection.Operation) error {
	return n.evidence.RecoverLifetime(ctx, operation)
}

func (n nativeOnlyEvidence) RecordedTerminal(ctx context.Context, operation *inspection.Operation) (bool, error) {
	return n.evidence.RecordedTerminal(ctx, operation)
}

type custodyCleanupFixture struct {
	store      *taskdir.Store
	supervisor task.SupervisorRef
	legacy     inspection.GroupRequestRecord
}

func newCustodyCleanupFixture(t *testing.T) custodyCleanupFixture {
	t.Helper()
	store := testsupport.NewInspectionStore(t)
	client, _ := testsupport.NewBlockingSupervisor(t, store.RootID)
	group := (pueue.InspectionIdentity{RootID: store.RootID}).Group()
	return custodyCleanupFixture{
		store: store, supervisor: client.Binding(),
		legacy: inspection.GroupRequestRecord{RootID: store.RootID, Supervisor: client.Binding(), Name: group, ParallelTasks: 1},
	}
}

// endedInspection writes one ended inspection whose native terminal evidence
// is valid and returns its queue row.
func (f custodyCleanupFixture) endedInspection(t *testing.T, fill string, id int64, binding inspection.Binding) pueue.Job {
	t.Helper()
	job := f.unpreparedInspection(t, fill, id, binding)
	// Native preparation retains the legacy worker exit, as maintenance does
	// before it authorizes removal.
	native := NewCleanup(nativeOnlyEvidence{evidence: &scriptedInspectionEvidence{}}, absentWorkspace)
	if err := native.PrepareEndedInspectionEvidence(t.Context(), f.store, f.legacy, strings.Repeat(fill, 32), job); err != nil {
		t.Fatal(err)
	}
	return job
}

// unpreparedInspection writes the same ended inspection without its
// identity-protected worker observation.
func (f custodyCleanupFixture) unpreparedInspection(t *testing.T, fill string, id int64, binding inspection.Binding) pueue.Job {
	t.Helper()
	request := testsupport.NewInspectionRequest(t, f.store)
	request.TaskID = strings.Repeat(fill, 32)
	digest := strings.Repeat("a", 64)
	binding.DefinitionRevision, binding.DefinitionSHA256 = "inspection-v1", digest
	binding.HelperExecutable, binding.HelperSHA256 = "/usr/bin/true", digest
	binding.WorkerExecutable, binding.WorkerSHA256 = "/usr/bin/true", digest
	binding.Supervisor = f.supervisor
	openedAt := time.Unix(100, 0).UTC()
	operation, err := inspection.OpenOperation(f.store, request, binding, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := operation.ClaimSubmission(openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultUnavailable, nil, openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(id); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	label := (pueue.InspectionIdentity{RootID: f.store.RootID, TaskID: request.TaskID}).Label()
	return pueue.Job{ID: id, Label: &label, State: pueue.StateEnded, Group: f.legacy.Name}
}

func absentWorkspace(context.Context, *taskdir.Store, inspection.GroupRequestRecord, string, pueue.Job) (bool, error) {
	return false, nil
}

func (f custodyCleanupFixture) authorize(t *testing.T, cleanup *Cleanup, jobs []pueue.Job) []int64 {
	t.Helper()
	plan, err := cleanup.AuthorizeSuccessfulCleanup(context.Background(), f.store.Root, f.supervisor, jobs)
	if err != nil {
		t.Fatal(err)
	}
	removed := slices.Clone(plan.RemoveIDs)
	slices.Sort(removed)
	return removed
}

func TestCleanupCustodyRetainsOnlyUnreleasedInspectionRows(t *testing.T) {
	f := newCustodyCleanupFixture(t)
	released := f.endedInspection(t, "a", 7, inspection.Binding{})
	faulted := f.endedInspection(t, "b", 8, inspection.Binding{})
	pending := f.endedInspection(t, "c", 9, inspection.Binding{})
	invalid := f.endedInspection(t, "e", 10, inspection.Binding{})
	workspaceLabel := (pueue.InspectionIdentity{RootID: f.store.RootID, TaskID: strings.Repeat("d", 32)}).Label()
	workspaceRow := pueue.Job{ID: 11, Label: &workspaceLabel, State: pueue.StateEnded, Group: f.legacy.Name}
	ordinaryLabel := "delegate:" + f.store.RootID + ":" + strings.Repeat("f", 32)
	ordinaryRow := pueue.Job{ID: 12, Label: &ordinaryLabel, State: pueue.StateEnded, Group: "default"}
	jobs := []pueue.Job{released, faulted, pending, invalid, workspaceRow, ordinaryRow}

	evidence := &scriptedInspectionEvidence{
		invalid:       map[string]bool{strings.Repeat("e", 32): true},
		released:      map[string]bool{strings.Repeat("a", 32): true, strings.Repeat("b", 32): true},
		removalFaults: map[string]error{strings.Repeat("b", 32): errors.New("custody pool root is missing")},
	}
	workspaceInspector := func(_ context.Context, _ *taskdir.Store, _ inspection.GroupRequestRecord, id string, _ pueue.Job) (bool, error) {
		return id == strings.Repeat("d", 32), nil
	}
	cleanup := NewCleanup(evidence, workspaceInspector)
	if got, want := f.authorize(t, cleanup, jobs), []int64{7, 11}; !slices.Equal(got, want) {
		t.Fatalf("removed rows %v, want only the released inspection and the workspace row %v", got, want)
	}
	wantCalls := []string{"authorize:" + strings.Repeat("a", 32), "authorize:" + strings.Repeat("b", 32), "authorize:" + strings.Repeat("c", 32)}
	if got := evidence.recorded(); !slices.Equal(got, wantCalls) {
		t.Fatalf("custody calls %v, want only validated inspection rows %v", got, wantCalls)
	}

	// Migration's validator never consults custody.
	for _, job := range []pueue.Job{faulted, pending} {
		if err := cleanup.InspectEndedJob(context.Background(), f.store, f.legacy, job); err != nil {
			t.Fatalf("migration validator of row %d: %v", job.ID, err)
		}
	}
	if got := evidence.recorded(); !slices.Equal(got, wantCalls) {
		t.Fatalf("migration validator consulted custody: %v", got)
	}

	// Without the extension the native policy is unchanged.
	native := NewCleanup(nativeOnlyEvidence{evidence: evidence}, workspaceInspector)
	if got, want := f.authorize(t, native, jobs), []int64{7, 8, 9, 11}; !slices.Equal(got, want) {
		t.Fatalf("native-only cleanup removed %v, want %v", got, want)
	}
}

// Identity-protected preparation keeps its native evidence work and never
// touches custody; the release phase owns that filesystem work.
func TestCleanupPreparationLeavesCustodyToReleasePhase(t *testing.T) {
	f := newCustodyCleanupFixture(t)
	managed := f.unpreparedInspection(t, "a", 7, inspection.Binding{
		RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
	})
	legacy := f.unpreparedInspection(t, "b", 8, inspection.Binding{})
	evidence := &scriptedInspectionEvidence{}
	cleanup := NewCleanup(evidence, absentWorkspace)
	if err := cleanup.PrepareTerminalEvidence(t.Context(), f.store.Root, f.supervisor, []pueue.Job{managed, legacy}); err != nil {
		t.Fatal(err)
	}
	if got := evidence.recorded(); len(got) != 0 {
		t.Fatalf("protected preparation called custody: %v", got)
	}
	if !workerObservationRecorded(t, f.store, strings.Repeat("b", 32)) {
		t.Fatal("protected preparation did not retain the legacy worker exit")
	}
}

// The release phase validates the canonical evidence of each unique ended
// inspection row and releases its custody, joining faults without stopping.
// It runs without queue identity, so it publishes no worker observation.
func TestCleanupReleaseInspectionCustodyPublishesNoObservation(t *testing.T) {
	f := newCustodyCleanupFixture(t)
	released := f.unpreparedInspection(t, "a", 7, inspection.Binding{})
	faulted := f.unpreparedInspection(t, "b", 8, inspection.Binding{})
	invalid := f.unpreparedInspection(t, "c", 9, inspection.Binding{})
	duplicate := f.unpreparedInspection(t, "d", 10, inspection.Binding{})
	duplicateCopy := duplicate
	duplicateCopy.ID = 11
	workspaceLabel := (pueue.InspectionIdentity{RootID: f.store.RootID, TaskID: strings.Repeat("e", 32)}).Label()
	workspaceRow := pueue.Job{ID: 12, Label: &workspaceLabel, State: pueue.StateEnded, Group: f.legacy.Name}
	ordinaryLabel := "delegate:" + f.store.RootID + ":" + strings.Repeat("f", 32)
	ordinaryRow := pueue.Job{ID: 13, Label: &ordinaryLabel, State: pueue.StateEnded, Group: "default"}
	custodyFault := errors.New("custody pool root is missing")
	evidence := &scriptedInspectionEvidence{
		invalid:       map[string]bool{strings.Repeat("c", 32): true},
		prepareFaults: map[string]error{strings.Repeat("b", 32): custodyFault},
	}
	cleanup := NewCleanup(evidence, absentWorkspace)
	jobs := []pueue.Job{released, faulted, invalid, duplicate, duplicateCopy, workspaceRow, ordinaryRow}
	err := cleanup.ReleaseInspectionCustody(t.Context(), f.store.Root, f.supervisor, jobs)
	if !errors.Is(err, custodyFault) || !strings.Contains(err.Error(), strings.Repeat("b", 32)) {
		t.Fatalf("custody release error=%v, want the joined fault naming its inspection", err)
	}
	want := []string{"prepare:" + strings.Repeat("a", 32), "prepare:" + strings.Repeat("b", 32)}
	if got := evidence.recorded(); !slices.Equal(got, want) {
		t.Fatalf("custody calls %v, want only validated unique inspection rows %v", got, want)
	}
	for _, fill := range []string{"a", "b"} {
		if workerObservationRecorded(t, f.store, strings.Repeat(fill, 32)) {
			t.Fatalf("custody release published a worker observation for inspection %s", fill)
		}
	}
	if err = NewCleanup(nativeOnlyEvidence{evidence: evidence}, absentWorkspace).ReleaseInspectionCustody(t.Context(), f.store.Root, f.supervisor, jobs); err != nil {
		t.Fatalf("release without a custody owner: %v", err)
	}
}

// A successful release reaches removal in the same pass; a faulted release
// retains only its own row behind the unchanged removal gate.
func TestCleanupReleasedCustodyReachesRemovalInSamePass(t *testing.T) {
	f := newCustodyCleanupFixture(t)
	released := f.endedInspection(t, "a", 7, inspection.Binding{})
	faulted := f.endedInspection(t, "b", 8, inspection.Binding{})
	evidence := &scriptedInspectionEvidence{
		releaseOnPrepare: true,
		prepareFaults:    map[string]error{strings.Repeat("b", 32): errors.New("custody pool identity differs")},
	}
	cleanup := NewCleanup(evidence, absentWorkspace)
	jobs := []pueue.Job{released, faulted}
	if got := f.authorize(t, cleanup, jobs); len(got) != 0 {
		t.Fatalf("rows %v were removable before custody release", got)
	}
	if err := cleanup.ReleaseInspectionCustody(t.Context(), f.store.Root, f.supervisor, jobs); err == nil {
		t.Fatal("custody fault was not reported")
	}
	if got, want := f.authorize(t, cleanup, jobs), []int64{7}; !slices.Equal(got, want) {
		t.Fatalf("removed rows %v after release, want only the released inspection %v", got, want)
	}
}

func workerObservationRecorded(t *testing.T, store *taskdir.Store, id string) bool {
	t.Helper()
	operation, err := inspection.LoadOperationContext(t.Context(), store, id)
	if err != nil {
		t.Fatal(err)
	}
	_, _, readErr := operation.ReadWorkerResultContext(t.Context())
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	return readErr == nil
}
