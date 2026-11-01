package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

const managedTaskCaptureID = "33333333333333333333333333333333"

// A task projection whose own bounded workspace view expires behind another
// holder of the workspace control lock has observed nothing about the
// workspace. It must stay undetermined and leave every task fact, including an
// offered continuation, unchanged.
func TestManagedTaskWorkspaceViewDeadlineRemainsUndetermined(t *testing.T) {
	store, resource, use := newManagedTaskUse(t)
	sealAppTaskUse(t, resource, use, managedTaskCaptureID)

	readable := resumableTaskProjection(store, use.td)
	fillRecordedTaskContext(store, use.td, os.Environ(), &readable)
	baseline := readable.response.Workspace
	requireReadableNonretainedView(t, baseline)
	if readable.response.Continuation.Resumable || readable.response.Continuation.ContinueCommand != "" || readable.response.Continuation.Reason != "managed workspace is "+baseline.State {
		t.Fatalf("proven nonretained description did not withhold continuation: %+v", readable.response.Continuation)
	}
	records := storeRecordDigests(t, store.Root)

	releaseLock, lockReleased := holdWorkspaceControlLock(t, store, baseline.ID)
	contended := resumableTaskProjection(store, use.td)
	before, envelope := contended, taskProjectionWithoutWorkspace(t, contended)
	fillRecordedTaskContext(store, use.td, os.Environ(), &contended)
	requireLockStillHeld(t, lockReleased)
	requireUnobservedWorkspaceView(t, contended.response.Workspace, baseline)
	if !contended.response.Continuation.Resumable || contended.response.Continuation.ContinueCommand == "" {
		t.Errorf("unknown workspace view revoked an offered continuation: %+v", contended.response.Continuation)
	}
	requireSameTaskEnvelope(t, "contended", before, contended, envelope)
	requireUnchangedRecords(t, "contended view", store.Root, records)

	releaseLock()
	<-lockReleased
	later := resumableTaskProjection(store, use.td)
	fillRecordedTaskContext(store, use.td, os.Environ(), &later)
	if !reflect.DeepEqual(later.response.Workspace, baseline) || !reflect.DeepEqual(later.response.Continuation, readable.response.Continuation) {
		t.Errorf("released view differs from the uncontended view: %+v %+v", later.response.Workspace, later.response.Continuation)
	}
	requireUnchangedRecords(t, "released view", store.Root, records)
}

// observationFailureCase is one load or description failure under the
// observer's own context, and whether it is only that observer's expired view.
type observationFailureCase struct {
	name, stage, reason string
	ctx                 context.Context
	err                 error
	expired             bool
}

// Only the observer's own expired view whose every cause is a deadline or a
// busy lock is unobserved. A live or cancelled context, or a concrete fault
// joined with the deadline, keeps the unreadable stop report.
func TestManagedTaskWorkspaceObservationFailureRequiresOwnExpiredView(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	live := t.Context()
	busy := fmt.Errorf("workspace control: %w", task.ErrLockBusy)
	storage := &fs.PathError{Op: "read", Path: "record", Err: syscall.EIO}
	closeFault := fmt.Errorf("close workspace resource: %w", storage)
	deadline := context.DeadlineExceeded
	for _, test := range []observationFailureCase{
		{name: "expired load deadline", stage: "load", reason: "workspace_load_timed_out", ctx: expired, err: deadline, expired: true},
		{name: "expired busy lock", stage: "describe", reason: "workspace_describe_failed", ctx: expired, err: busy, expired: true},
		{name: "expired deadline joined with busy close", stage: "describe", reason: "workspace_describe_timed_out", ctx: expired, err: errors.Join(deadline, busy), expired: true},
		{name: "live context deadline", stage: "load", reason: "workspace_load_timed_out", ctx: live, err: deadline},
		{name: "live context busy lock", stage: "describe", reason: "workspace_describe_failed", ctx: live, err: busy},
		{name: "cancelled context deadline", stage: "load", reason: "workspace_load_timed_out", ctx: cancelled, err: deadline},
		{name: "expired context cancellation", stage: "describe", reason: "workspace_describe_failed", ctx: expired, err: context.Canceled},
		{name: "task identity", stage: "describe", reason: "workspace_describe_identity_mismatch", ctx: expired, err: errors.Join(deadline, task.ErrIdentityMismatch)},
		{name: "workspace identity", stage: "load", reason: "workspace_load_identity_mismatch", ctx: expired, err: errors.Join(deadline, workspace.ErrIdentity)},
		{name: "evidence", stage: "describe", reason: "workspace_describe_evidence_fault", ctx: expired, err: errors.Join(deadline, task.ErrEvidenceFault)},
		{name: "permission", stage: "load", reason: "workspace_load_permission_denied", ctx: expired, err: errors.Join(deadline, &fs.PathError{Op: "open", Path: "record", Err: os.ErrPermission})},
		{name: "missing state", stage: "describe", reason: "workspace_describe_state_unavailable", ctx: expired, err: errors.Join(deadline, os.ErrNotExist)},
		{name: "storage", stage: "describe", reason: "workspace_describe_timed_out", ctx: expired, err: errors.Join(deadline, storage)},
		{name: "close", stage: "describe", reason: "workspace_describe_timed_out", ctx: expired, err: errors.Join(deadline, closeFault)},
		{name: "busy close", stage: "describe", reason: "workspace_describe_failed", ctx: expired, err: errors.Join(busy, closeFault)},
	} {
		t.Run(test.name, func(t *testing.T) { requireObservationFailureProjection(t, test) })
	}
	if expiredWorkspaceView(expired, nil) {
		t.Error("an absent error was treated as an expired view")
	}
}

// requireObservationFailureProjection checks the task projection and the
// workspace command response, which share one expired-view classification.
func requireObservationFailureProjection(t *testing.T, test observationFailureCase) {
	t.Helper()
	unreadable := WorkspaceResponse{ID: strings.Repeat("e", 32), Isolation: "git-worktree", State: "unreadable", ReasonCode: "workspace_observation_failed", NextAction: "stop_and_report", EvidenceState: "unknown"}
	want := unreadable
	want.ReasonCode = test.reason
	if test.expired {
		want.State, want.NextAction = "undetermined", "watch_workspace"
	}
	got := unreadable
	projectWorkspaceObservationFailure(test.ctx, test.stage, test.err, &got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("task workspace projection = %+v, want %+v", got, want)
	}
	response := newResponse("workspace")
	response.Workspace = &WorkspaceResponse{ID: unreadable.ID}
	result := finishWorkspaceCommandResponse(test.ctx, response, nil, test.err)
	if wantCode := map[bool]int{true: 3, false: 1}[test.expired]; result.code != wantCode {
		t.Errorf("workspace command code = %d, want %d: %+v", result.code, wantCode, result.response.Workspace)
	}
}

// newManagedTaskUse binds a fresh task to its own reserved use of a managed
// workspace whose source is recorded. Callers seal the use's capture.
func newManagedTaskUse(t *testing.T) (*taskdir.Store, *workspace.Resource, appTaskUse) {
	t.Helper()
	store, resource := testsupport.NewResource(t)
	template, _ := newAppTaskInStore(t, store, strings.Repeat("f", 32), false, nil)
	t.Cleanup(func() {
		if closeErr := template.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err := resource.RecordSource(t.Context(), workspace.Source{Root: "/fixture/source", CommonDir: "/fixture/source/.git", GitDir: "/fixture/source/.git", RelativeCwd: ".", BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1"}); err != nil {
		t.Fatal(err)
	}
	return store, resource, seedAppTaskUse(t, store, template, resource, resource.Request().TaskID)
}

// resumableTaskProjection is a collect result for a budget-stopped turn that
// offers continuation, as tasklifecycle reports it before the workspace view.
func resumableTaskProjection(store *taskdir.Store, td *taskdir.TaskDir) commandResult {
	response := newResponse("collect")
	response.RootID, response.TaskID = store.RootID, td.TaskID
	response.Status, response.Publication = string(tasklifecycle.ContinuationTimedOut), task.PublicationPending.String()
	response.Continuation = &ContinuationResponse{
		Resumable: true, Mode: string(commonprovider.ContinuationNative), PredecessorID: td.TaskID,
		ContinueCommand: continuationCommand(store.Root, td.TaskID, "", nil),
	}
	return commandResult{response: response}
}

func requireReadableNonretainedView(t *testing.T, view *WorkspaceResponse) {
	t.Helper()
	if view == nil || view.Capture == nil || view.Capture.OperationID != managedTaskCaptureID || view.State == "retained" || view.State == "undetermined" {
		t.Fatalf("uncontended view was not this task's readable nonretained description: %+v", view)
	}
}

// requireUnobservedWorkspaceView checks that an expired view keeps only the
// workspace identity and typed cause it already knew, and invents nothing.
func requireUnobservedWorkspaceView(t *testing.T, got, baseline *WorkspaceResponse) {
	t.Helper()
	if got == nil {
		t.Fatal("managed task lost its workspace projection")
	}
	if got.ID != baseline.ID || got.Isolation != baseline.Isolation || got.ReasonCode != "workspace_load_timed_out" {
		t.Errorf("expired view lost its identity or typed cause: %+v", got)
	}
	if got.State != "undetermined" || got.EvidenceState != "unknown" || got.NextAction != "watch_workspace" {
		t.Errorf("own expired view reported state=%q next=%q evidence=%q; want undetermined/watch_workspace/unknown", got.State, got.NextAction, got.EvidenceState)
	}
	if got.Path != "" || got.CanonicalCwd != "" || got.SourceRoot != "" || got.Capture != nil || len(got.Artifacts) != 0 {
		t.Errorf("unobserved workspace view invented evidence: %+v", got)
	}
}

func requireLockStillHeld(t *testing.T, released <-chan struct{}) {
	t.Helper()
	select {
	case <-released:
		t.Fatal("control lock holder finished before the view returned")
	default:
	}
}

func requireSameTaskEnvelope(t *testing.T, stage string, baseline, observed commandResult, envelope string) {
	t.Helper()
	if observed.code != baseline.code || fmt.Sprint(observed.err) != fmt.Sprint(baseline.err) || taskProjectionWithoutWorkspace(t, observed) != envelope {
		t.Errorf("%s workspace view changed the task envelope: code=%d err=%v response=%s", stage, observed.code, observed.err, taskProjectionWithoutWorkspace(t, observed))
	}
}

func taskProjectionWithoutWorkspace(t *testing.T, result commandResult) string {
	t.Helper()
	response := result.response
	response.Workspace = nil
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// holdWorkspaceControlLock holds the workspace control lock from another
// handle until release is called. Cleanup releases and joins the holder even
// when the test stops early.
func holdWorkspaceControlLock(t *testing.T, store *taskdir.Store, workspaceID string) (release func(), released <-chan struct{}) {
	t.Helper()
	control, err := store.OpenWorkspace(t.Context(), workspaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	entered, releaseLock, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(releaseLock) }) }
	var lockErr error
	go func() {
		defer close(finished)
		lockErr = control.WithLock(t.Context(), func(*taskdir.ControlTransaction) error {
			close(entered)
			<-releaseLock
			return nil
		})
	}()
	t.Cleanup(func() {
		release()
		<-finished
		if lockErr != nil {
			t.Error(lockErr)
		}
	})
	select {
	case <-entered:
	case <-finished:
		t.Fatalf("workspace control lock was not held: %v", lockErr)
	}
	return release, finished
}

func requireUnchangedRecords(t *testing.T, stage, root string, want map[string]string) {
	t.Helper()
	if changed := changedRecords(t, root, want); len(changed) != 0 {
		t.Errorf("%s changed durable records: %s", stage, strings.Join(changed, ", "))
	}
}

// changedRecords names each root-relative file whose digest differs from want.
func changedRecords(t *testing.T, root string, want map[string]string) []string {
	t.Helper()
	got := storeRecordDigests(t, root)
	paths := maps.Clone(got)
	maps.Copy(paths, want)
	var changed []string
	for path := range paths {
		if got[path] != want[path] {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				t.Fatal(err)
			}
			changed = append(changed, fmt.Sprintf("%s(before=%t after=%t)", relative, want[path] != "", got[path] != ""))
		}
	}
	slices.Sort(changed)
	return changed
}

func storeRecordDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digests[path] = task.ComputeSHA256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return digests
}
