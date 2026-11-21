package tasklifecycle

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type guardedContinuationResources struct{ checks atomic.Int32 }

func (r *guardedContinuationResources) Check(context.Context, resourceadmission.TargetResolver) (resourceadmission.Assessment, error) {
	r.checks.Add(1)
	return clearAssessment(time.Now()), nil
}

func (*guardedContinuationResources) Scope(context.Context, resourceadmission.TargetResolver) (resourceadmission.Scope, error) {
	return resourceadmission.Scope{}, nil
}

func guardedContinuationFixture(t *testing.T) (teamDispatchFixture, DispatchRequest, ContinuationOptions) {
	t.Helper()
	fixture := newPathContinuationFixture(t)
	identity, err := fixture.store.RecordedTaskIdentityContext(t.Context(), fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := identity.ConnectorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	selection := &taskdir.ConnectorTaskSelection{
		RootToken: receipt.RootToken, ProjectSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(receipt.ProjectToken))),
		Origin: receipt.Origin.Clone(), SpecSHA256: receipt.SpecSHA256,
		WorkspaceConstraint: receipt.WorkspaceConstraint.Clone(),
	}
	request := teamContinuationReplayRequest(fixture)
	request.TaskID, request.ResumeTask = fixture.request.TaskID, ""
	request.Origin = fixture.request.Origin.Clone()
	request.Origin.OperationID = "successor"
	request.Origin.GatewayRequestSHA256 = strings.Repeat("e", 64)
	request.ConnectorContext = fixture.request.ConnectorContext.Clone()
	options := pathContinuationOptions(t, fixture)
	options.Resources = &guardedContinuationResources{}
	return fixture, request, ContinuationOptions{Dispatch: options, SuccessorTaskID: strings.Repeat("c", 32), PredecessorGuard: selection}
}

func TestGuardedContinuationRejectsPredecessorMismatchBeforeMutation(t *testing.T) {
	changes := []struct {
		name string
		edit func(*taskdir.ConnectorTaskSelection)
	}{
		{"root", func(s *taskdir.ConnectorTaskSelection) { s.RootToken = strings.Repeat("0", 64) }},
		{"project hash", func(s *taskdir.ConnectorTaskSelection) { s.ProjectSHA256 = strings.Repeat("0", 64) }},
		{"origin", func(s *taskdir.ConnectorTaskSelection) { s.Origin.OperationID = "other-predecessor" }},
		{"spec", func(s *taskdir.ConnectorTaskSelection) { s.SpecSHA256 = strings.Repeat("0", 64) }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			fixture, request, options := guardedContinuationFixture(t)
			change.edit(options.PredecessorGuard)
			before := continuationStoreSnapshot(t, fixture.store.Root)
			result := ContinueContext(t.Context(), request, options)
			if result.Phase != ContinueLoadPredecessor || (!errors.Is(result.Failure, task.ErrRequestConflict) && !errors.Is(result.Failure, task.ErrIdentityMismatch)) {
				t.Fatalf("predecessor guard did not reject before continuation: %+v, %v", result, result.Failure)
			}
			resources, ok := options.Dispatch.Resources.(*guardedContinuationResources)
			if !ok || resources.checks.Load() != 0 {
				t.Fatal("incorrect predecessor reached resource preparation")
			}
			if !reflect.DeepEqual(before, continuationStoreSnapshot(t, fixture.store.Root)) {
				t.Fatal("incorrect predecessor guard mutated durable state")
			}
		})
	}
}

func TestGuardedContinuationRequiresStableSuccessorAndFreshOrigin(t *testing.T) {
	changes := []struct {
		name string
		edit func(*DispatchRequest, *ContinuationOptions)
	}{
		{"missing successor", func(_ *DispatchRequest, o *ContinuationOptions) { o.SuccessorTaskID = "" }},
		{"predecessor ID", func(r *DispatchRequest, o *ContinuationOptions) { o.SuccessorTaskID = r.TaskID }},
		{"invalid successor", func(_ *DispatchRequest, o *ContinuationOptions) { o.SuccessorTaskID = "invalid" }},
		{"missing predecessor spec", func(_ *DispatchRequest, o *ContinuationOptions) { o.PredecessorGuard.SpecSHA256 = "" }},
		{"missing origin", func(r *DispatchRequest, _ *ContinuationOptions) { r.Origin = nil }},
		{"invalid origin", func(r *DispatchRequest, _ *ContinuationOptions) { r.Origin.OperationID = "invalid operation" }},
		{"same operation with new digest", func(r *DispatchRequest, o *ContinuationOptions) {
			r.Origin.OperationID = o.PredecessorGuard.Origin.OperationID
		}},
		{"both project forms", func(_ *DispatchRequest, o *ContinuationOptions) { o.PredecessorGuard.ProjectToken = "also supplied" }},
	}
	fixture, original, originalOptions := guardedContinuationFixture(t)
	before := continuationStoreSnapshot(t, fixture.store.Root)
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			request, options := original, originalOptions
			request.Origin = original.Origin.Clone()
			selection := *originalOptions.PredecessorGuard
			options.PredecessorGuard = &selection
			change.edit(&request, &options)
			result := ContinueContext(t.Context(), request, options)
			if result.Failure == nil || result.Phase != ContinueValidation || result.Dispatch != nil {
				t.Fatalf("unstable continuation identity was accepted: %+v", result)
			}
			if !reflect.DeepEqual(before, continuationStoreSnapshot(t, fixture.store.Root)) {
				t.Fatal("unstable continuation identity changed durable state")
			}
		})
	}
}

func TestGuardedContinuationLostResponseAndConcurrentReplayKeepOneSuccessor(t *testing.T) {
	fixture, request, options := guardedContinuationFixture(t)
	sealTeamPredecessor(t, fixture)
	candidate := request
	candidate.TaskID, candidate.ResumeTask = options.SuccessorTaskID, fixture.request.TaskID
	winner := teamContinuationReplayRecord(t, fixture, candidate)
	winner.Origin, winner.ConnectorContext = request.Origin.Clone(), request.ConnectorContext.Clone()
	td := persistTeamContinuationReplay(t, fixture, winner)
	// Finish the existing-task owner's terminal session release, then model a
	// lost response by repeating that exact request concurrently.
	first := ContinueContext(t.Context(), request, options)
	requireGuardedContinuationReplay(t, first, winner.TaskID)
	before := continuationStoreSnapshot(t, fixture.store.Root)
	replayContext, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	options.Dispatch.OpenStore = concurrentContinuationStoreOpener(replayContext, options.Dispatch.OpenStore)
	results := make(chan ContinuationResult, 2)
	var callers sync.WaitGroup
	for range 2 {
		callers.Go(func() { results <- ContinueContext(replayContext, request, options) })
	}
	callers.Wait()
	close(results)
	for result := range results {
		requireGuardedContinuationReplay(t, result, winner.TaskID)
	}
	if !reflect.DeepEqual(before, continuationStoreSnapshot(t, fixture.store.Root)) {
		t.Fatal("concurrent replay changed saved task/session records")
	}
	changed := request
	changed.Config.Model = "conflicting-successor-model"
	conflict := ContinueContext(t.Context(), changed, options)
	if conflict.Dispatch == nil || !errors.Is(conflict.Dispatch.Err(), task.ErrRequestConflict) {
		t.Fatalf("same-ID conflicting continuation was not refused: %+v, %v", conflict, conflict.Failure)
	}
	saved, err := td.RecordedRequest()
	if err != nil || !reflect.DeepEqual(saved, &winner) {
		t.Fatalf("conflicting retry changed winner: %+v, %v", saved, err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.store.Root, "tasks"))
	if err != nil || len(entries) != 4 {
		t.Fatalf("replay allocated another successor: entries=%d, err=%v", len(entries), err)
	}
}

// The first caller cannot pass predecessor acquisition until a second caller
// enters it. The caller context also releases the gate on fixture failure.
func concurrentContinuationStoreOpener(ctx context.Context, open TaskStoreOpener) TaskStoreOpener {
	var entrants atomic.Int32
	ready := make(chan struct{})
	return func(root string, create bool) (*taskdir.Store, error) {
		switch entrants.Add(1) {
		case 1:
			select {
			case <-ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case 2:
			close(ready)
		}
		return open(root, create)
	}
}

func requireGuardedContinuationReplay(t *testing.T, result ContinuationResult, id string) {
	t.Helper()
	if result.Failure != nil || result.Dispatch == nil {
		t.Fatalf("same-ID replay did not dispatch existing successor: %+v, %v", result, result.Failure)
	}
	if result.Dispatch.TaskID != id {
		t.Fatalf("same-ID replay selected %q, want %q", result.Dispatch.TaskID, id)
	}
	requireTeamContinuationReplayResult(t, *result.Dispatch, true)
}
