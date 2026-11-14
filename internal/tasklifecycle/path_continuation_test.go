package tasklifecycle

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newPathContinuationFixture(t *testing.T) teamDispatchFixture {
	t.Helper()
	fixture := newTeamDispatchFixture(t)
	if err := fixture.store.EnableWorkspaceConnectorContext(t.Context(), true, true); err != nil {
		t.Fatal(err)
	}
	rootToken, err := fixture.store.RootTokenContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.OriginBarrierContext(t.Context(), "gateway", "initial", "", "generation", rootToken); err != nil {
		t.Fatal(err)
	}
	inventory, err := fixture.store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.OriginReopenContext(t.Context(), "gateway", "initial", "generation", inventory.CompletenessReceipt, rootToken); err != nil {
		t.Fatal(err)
	}
	project, err := taskdir.ObservePathProjectContext(t.Context(), fixture.request.CanonicalCwd)
	if err != nil {
		t.Fatal(err)
	}
	token, err := taskdir.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	request, meta := *fixture.request, *fixture.meta
	request.TaskID, meta.TaskID = strings.Repeat("d", 32), strings.Repeat("d", 32)
	meta.RunnerExecutable, meta.RunnerOwnership = filepath.Join(t.TempDir(), "runner"), task.RunnerOwnershipCustom
	supervisorfixture.WriteExecutable(t, meta.RunnerExecutable, "#!/bin/sh\nexit 64\n")
	request.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "predecessor", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
	request.ConnectorContext = &task.ConnectorContext{RootToken: rootToken, Project: project, WorkspaceConstraint: &task.WorkspaceConstraint{Token: token, RelativeProject: "."}}
	td, err := fixture.store.CreateTask(request.TaskID, &request, fixture.brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fixture.td, fixture.request, fixture.meta = td, &request, &meta
	return fixture
}

func TestBuildPathContinuationInheritsConstraintWithoutOrigin(t *testing.T) {
	fixture := newPathContinuationFixture(t)
	successor, err := BuildContinuationRequest(DispatchRequest{}, fixture.store.Root, fixture.request, nil, fixture.brief)
	if err != nil || successor.Origin != nil || !reflect.DeepEqual(successor.ConnectorContext, fixture.request.ConnectorContext) {
		t.Fatalf("local continuation lost binding or inherited origin: %+v, %v", successor, err)
	}
	successor.ConnectorContext.WorkspaceConstraint.RelativeProject = "changed"
	if fixture.request.ConnectorContext.WorkspaceConstraint.RelativeProject != "." {
		t.Fatal("continuation shares predecessor workspace constraint")
	}
	origin := fixture.request.Origin.Clone()
	origin.OperationID = "successor"
	requested := DispatchRequest{Origin: origin, ConnectorContext: fixture.request.ConnectorContext.Clone()}
	successor, err = BuildContinuationRequest(requested, fixture.store.Root, fixture.request, nil, fixture.brief)
	if err != nil || !reflect.DeepEqual(successor.Origin, origin) || successor.Origin == origin {
		t.Fatalf("new caller origin was replaced or shared: %+v, %v", successor.Origin, err)
	}
}

func pathContinuationOptions(t *testing.T, fixture teamDispatchFixture) DispatchOptions {
	t.Helper()
	options := teamResumeOptions(t, fixture)
	options.OpenStore = func(root string, _ bool) (*taskdir.Store, error) { return taskdir.OpenStore(root) }
	options.Resources = &scriptedResources{t: t, assessments: []resourceadmission.Assessment{clearAssessment(time.Now())}}
	return options
}

func TestPathContinuationAndDirectResumeAfterDirectoryReplacement(t *testing.T) {
	for _, caller := range []string{"continue", "direct resume"} {
		for _, attributed := range []bool{false, true} {
			t.Run(caller+"/"+map[bool]string{false: "local", true: "new origin"}[attributed], func(t *testing.T) {
				runPathContinuationReplay(t, caller, attributed)
			})
		}
	}
}

func runPathContinuationReplay(t *testing.T, caller string, attributed bool) {
	t.Helper()
	fixture := newPathContinuationFixture(t)
	sealTeamPredecessor(t, fixture)
	requested := teamContinuationReplayRequest(fixture)
	requested.ConnectorContext = fixture.request.ConnectorContext.Clone()
	if attributed {
		requested.Origin = fixture.request.Origin.Clone()
		requested.Origin.OperationID = "successor"
	}
	winner := teamContinuationReplayRecord(t, fixture, requested)
	winner.ConnectorContext, winner.Origin = requested.ConnectorContext.Clone(), requested.Origin.Clone()
	td := persistTeamContinuationReplay(t, fixture, winner)
	before, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(fixture.request.CanonicalCwd, fixture.request.CanonicalCwd+"-old"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(fixture.request.CanonicalCwd, 0o700); err != nil {
		t.Fatal(err)
	}
	options := pathContinuationOptions(t, fixture)
	// Local requests omit the inherited execution context entirely.
	if !attributed {
		requested.ConnectorContext = nil
	}
	var result DispatchResult
	if caller == "continue" {
		requested.TaskID, requested.ResumeTask = fixture.request.TaskID, ""
		continued := ContinueContext(t.Context(), requested, ContinuationOptions{Dispatch: options, SuccessorTaskID: winner.TaskID})
		if continued.Failure != nil || continued.Dispatch == nil {
			t.Fatalf("continuation did not reach idempotent successor: %+v, %v", continued, continued.Failure)
		}
		result = *continued.Dispatch
	} else {
		result = DispatchContext(t.Context(), requested, options)
	}
	if result.Err() != nil || result.Existing == nil || result.Creation != nil {
		t.Fatalf("path-bound successor replay failed: %+v, %v", result, result.Err())
	}
	after, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("replay changed immutable successor: %v", err)
	}
}

func continuationStoreSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPathContinuationRejectsRetargetBeforePredecessorMutation(t *testing.T) {
	for _, caller := range []string{"continue", "direct resume", "predecessor API"} {
		t.Run(caller, func(t *testing.T) {
			fixture := newPathContinuationFixture(t)
			if err := fixture.td.ClaimSession(fixture.request.Provider, "recorded-conversation"); err != nil {
				t.Fatal(err)
			}
			sealTeamPredecessor(t, fixture)
			before := continuationStoreSnapshot(t, fixture.store.Root)
			requested := teamContinuationReplayRequest(fixture)
			requested.Origin, requested.ConnectorContext = fixture.request.Origin.Clone(), fixture.request.ConnectorContext.Clone()
			requested.Origin.OperationID = "new-caller"
			requested.ConnectorContext.WorkspaceConstraint.RelativeProject = "other"
			options := pathContinuationOptions(t, fixture)
			var err error
			switch caller {
			case "continue":
				requested.TaskID = fixture.request.TaskID
				err = ContinueContext(t.Context(), requested, ContinuationOptions{Dispatch: options}).Failure
			case "direct resume":
				err = DispatchContext(t.Context(), requested, options).Err()
			case "predecessor API":
				record := teamContinuationReplayRecord(t, fixture, requested)
				record.ConnectorContext = nil // Bypass both inheriting builders.
				_, err = ResolvePredecessorContext(t.Context(), fixture.store, PredecessorOptions{Admission: options.staticAdmission()}, record, fixture.request.TaskID)
			}
			if !errors.Is(err, task.ErrRequestConflict) {
				t.Fatalf("retargeted continuation error = %v", err)
			}
			if after := continuationStoreSnapshot(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected continuation changed predecessor or session records")
			}
		})
	}
}
