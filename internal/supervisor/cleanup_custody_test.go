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
	return e.prepareFaults[id]
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
	job := pueue.Job{ID: id, Label: &label, State: pueue.StateEnded, Group: f.legacy.Name}
	// Native preparation retains the legacy worker exit, as maintenance does
	// before it authorizes removal.
	native := NewCleanup(nativeOnlyEvidence{evidence: &scriptedInspectionEvidence{}}, absentWorkspace)
	if err = native.PrepareEndedInspectionEvidence(t.Context(), f.store, f.legacy, request.TaskID, job); err != nil {
		t.Fatal(err)
	}
	return job
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

func TestCleanupPreparationReleasesCustodyWithoutChangingItsResult(t *testing.T) {
	f := newCustodyCleanupFixture(t)
	managed := f.endedInspection(t, "a", 7, inspection.Binding{
		RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
	})
	legacy := f.endedInspection(t, "b", 8, inspection.Binding{})
	evidence := &scriptedInspectionEvidence{prepareFaults: map[string]error{
		strings.Repeat("a", 32): errors.New("custody pool root is missing"),
		strings.Repeat("b", 32): errors.New("custody pool identity differs"),
	}}
	cleanup := NewCleanup(evidence, absentWorkspace)
	for _, job := range []pueue.Job{managed, legacy} {
		id, _, _, err := LegacyJobTaskID(*job.Label, f.store.RootID)
		if err != nil {
			t.Fatal(err)
		}
		if err = cleanup.PrepareEndedInspectionEvidence(t.Context(), f.store, f.legacy, id, job); err != nil {
			t.Fatalf("custody fault changed preparation of row %d: %v", job.ID, err)
		}
	}
	want := []string{"prepare:" + strings.Repeat("a", 32), "prepare:" + strings.Repeat("b", 32)}
	if got := evidence.recorded(); !slices.Equal(got, want) {
		t.Fatalf("custody preparation calls %v, want %v, including the guardian-wait row", got, want)
	}
}
