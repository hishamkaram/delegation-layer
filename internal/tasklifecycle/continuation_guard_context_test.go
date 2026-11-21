package tasklifecycle

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestGuardedContinuationInheritsPhysicalContextBeforeSuccessorReplay(t *testing.T) {
	fixture, request, options := guardedPhysicalContextFixture(t)
	sealTeamPredecessor(t, fixture)
	candidate := request
	candidate.TaskID, candidate.ResumeTask = options.SuccessorTaskID, fixture.request.TaskID
	winner := teamContinuationReplayRecord(t, fixture, candidate)
	winner.Origin, winner.ConnectorContext = request.Origin.Clone(), fixture.request.ConnectorContext.Clone()
	successor := persistTeamContinuationReplay(t, fixture, winner)
	request.ConnectorContext = nil
	result := ContinueContext(t.Context(), request, options)
	requireGuardedContinuationReplay(t, result, winner.TaskID)
	saved, err := successor.RecordedRequest()
	if err != nil || !reflect.DeepEqual(saved.ConnectorContext, fixture.request.ConnectorContext) {
		t.Fatalf("guarded continuation lost the saved physical constraint: %+v, %v", saved, err)
	}
	if request.ConnectorContext != nil {
		t.Fatal("continuation changed the caller's omitted execution context")
	}
}

func TestGuardedContinuationRejectsExplicitContextRetargetBeforeResourcesOrMutation(t *testing.T) {
	cases := []struct {
		name     string
		physical bool
		change   func(*task.ConnectorContext)
	}{
		{"physical project", true, func(binding *task.ConnectorContext) { binding.Project.Inode++ }},
		{"path root", false, func(binding *task.ConnectorContext) { binding.RootToken = strings.Repeat("0", 64) }},
		{"path workspace", false, func(binding *task.ConnectorContext) { binding.WorkspaceConstraint.RelativeProject = "other" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			newFixture := guardedContinuationFixture
			if test.physical {
				newFixture = guardedPhysicalContextFixture
			}
			fixture, request, options := newFixture(t)
			request.ConnectorContext = fixture.request.ConnectorContext.Clone()
			test.change(request.ConnectorContext)
			requested := request.ConnectorContext.Clone()
			before := continuationStoreSnapshot(t, fixture.store.Root)
			result := ContinueContext(t.Context(), request, options)
			resources, ok := options.Dispatch.Resources.(*guardedContinuationResources)
			if !ok || resources.checks.Load() != 0 {
				t.Error("retargeted execution context reached resource preparation")
			}
			if !errors.Is(result.Failure, task.ErrRequestConflict) || result.Dispatch != nil {
				t.Errorf("guarded context retarget was not rejected before successor admission: %+v, %v", result, result.Failure)
			}
			if !reflect.DeepEqual(request.ConnectorContext, requested) || !reflect.DeepEqual(before, continuationStoreSnapshot(t, fixture.store.Root)) {
				t.Fatal("rejected context retarget changed caller input or durable task/session state")
			}
		})
	}
}

func guardedPhysicalContextFixture(t *testing.T) (teamDispatchFixture, DispatchRequest, ContinuationOptions) {
	t.Helper()
	fixture, request, options := guardedContinuationFixture(t)
	physical, err := taskdir.ObserveProjectContext(t.Context(), fixture.request.CanonicalCwd)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, meta := *fixture.request, *fixture.meta
	predecessor.TaskID, meta.TaskID = strings.Repeat("e", 32), strings.Repeat("e", 32)
	predecessor.ConnectorContext = &task.ConnectorContext{RootToken: predecessor.ConnectorContext.RootToken, Project: physical}
	td, err := fixture.store.CreateTask(predecessor.TaskID, &predecessor, fixture.brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fixture.td, fixture.request, fixture.meta = td, &predecessor, &meta
	options.Dispatch = pathContinuationOptions(t, fixture)
	options.Dispatch.Resources = &guardedContinuationResources{}
	identity, err := fixture.store.RecordedTaskIdentityContext(t.Context(), predecessor.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := identity.ConnectorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	options.PredecessorGuard = &taskdir.ConnectorTaskSelection{
		RootToken: receipt.RootToken, ProjectSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(receipt.ProjectToken))),
		Origin: receipt.Origin.Clone(), SpecSHA256: receipt.SpecSHA256,
	}
	request.TaskID, request.ResumeTask, request.ConnectorContext = predecessor.TaskID, "", predecessor.ConnectorContext.Clone()
	return fixture, request, options
}
