package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func TestDispatchWorkflowMissingStorePortRefusesBeforeState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	request := tasklifecycle.DispatchRequest{Root: root, Cwd: t.TempDir(), BriefBytes: []byte("brief")}
	result := tasklifecycle.Dispatch(request, tasklifecycle.DispatchOptions{})
	if !errors.Is(result.Err(), task.ErrEvidenceFault) || result.Phase != tasklifecycle.DispatchOpenState {
		t.Fatalf("missing store owner did not stop dispatch: %+v", result)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing store owner created state: %v", err)
	}
}

func TestContinuationMissingProfilePortRetainsPreparedJournal(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	before, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
	testsupport.RequireAppTest(t, err)
	options := dispatchOptions(Dependencies{}, true)
	options.ContinuationProfiles = nil
	_, _, err = tasklifecycle.ContinuationOptionsForTask(store, options, req.TaskID)
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("missing profile owner did not stop continuation: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
	testsupport.RequireAppTest(t, err)
	if string(before) != string(after) {
		t.Fatal("missing profile owner changed prepared evidence")
	}
	if _, err = td.ReadSubmission(); !task.RecordAbsent(err) {
		t.Fatalf("missing profile owner created submission authority: %v", err)
	}
}

func TestDispatchWorkflowClosesRealStoreAfterCandidateRefusal(t *testing.T) {
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.RequireAppTest(t, err)
	fixture := preflightFixture{root: filepath.Join(temporary, "state"), cwd: t.TempDir(), prepareFault: commonprovider.ErrAuthenticationBlocked}
	deps := Dependencies{Catalog: preflightFixtureCatalog(t, fixture.candidate)}
	options := dispatchOptions(deps, false)
	var opened *taskdir.Store
	options.OpenStore = func(root string, create bool) (*taskdir.Store, error) {
		if root != fixture.root || !create {
			t.Fatalf("wrong store boundary: %q/%t", root, create)
		}
		var err error
		opened, err = openStore(root, deps.storeDependencies(), create)
		return opened, err
	}
	request := tasklifecycle.DispatchRequest{Root: fixture.root, Cwd: fixture.cwd, Provider: "fixture:preflight", BriefBytes: []byte("brief"), Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"}}
	observed := tasklifecycle.Dispatch(request, options)
	if opened == nil {
		t.Fatal("dispatch never opened real store")
	}
	t.Cleanup(func() { testsupport.RequireAppTest(t, opened.Close()) })
	result := dispatchCommandResult(observed)
	if result.code != 2 || result.response.Status != preflightStatusBlocked || result.response.TaskRecord != "not_created" || !errors.Is(result.err, fixture.prepareFault) || fixture.prepareCalls != 1 {
		t.Fatalf("candidate refusal changed dispatch evidence: %+v, calls=%d", result, fixture.prepareCalls)
	}
	if err := opened.ProbeFilesystemSupport(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("workflow retained its store after refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "tasks", observed.TaskID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate refusal created ordinary task: %v", err)
	}
}

func TestContinuationProfilesPreserveInjectedCurrentAndHistoricalCatalog(t *testing.T) {
	for _, injected := range []bool{false, true} {
		t.Run(map[bool]string{false: "historical", true: "injected current"}[injected], func(t *testing.T) {
			customFault, historicalFault := errors.New("custom current"), errors.New("recorded contract")
			customCalls, historicalCalls := 0, 0
			current := func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
				customCalls++
				return commonprovider.ProfileCandidate{}, customFault
			}
			catalog := continuationProfilePortCatalog(t, current, func(task.TaskRecord, task.MetaRecord) (commonprovider.ProfileCandidate, error) {
				historicalCalls++
				return commonprovider.ProfileCandidate{}, historicalFault
			})
			deps := Dependencies{Catalog: catalog}
			if injected {
				deps.PrepareCandidate = current
			}
			root, cwd := t.TempDir(), t.TempDir()
			req, err := admissioncore.BuildRequest(strings.Repeat("a", 32), strings.Repeat("b", 32), "fixture:preflight", cwd, task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"}, []byte("brief"), nil)
			testsupport.RequireAppTest(t, err)
			profiles := dispatchOptions(deps, true).ContinuationProfiles(task.MetaRecord{})
			_, err = profiles.Prepare(root, req)
			want, calls := historicalFault, historicalCalls
			if injected {
				want, calls = customFault, customCalls
			}
			if !errors.Is(err, want) || calls != 1 || customCalls+historicalCalls != 1 {
				t.Fatalf("wrong continuation factory: %v, current/history=%d/%d", err, customCalls, historicalCalls)
			}
		})
	}
}

func continuationProfilePortCatalog(t *testing.T, current commonprovider.PrepareCandidate, historical commonprovider.PrepareExistingCandidate) commonprovider.Catalog {
	t.Helper()
	description := preflightFixtureCatalog(t, current).Descriptions()[0]
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{Description: description, Prepare: current, PrepareExisting: historical, Interpreters: []predicate.Interpreter{autoTestInterpreter{reference: preflightFixturePredicate(description.ID)}}})
	testsupport.RequireAppTest(t, err)
	return catalog
}

func TestWorkspaceRuntimeConstructorSealsConfigurationAndPreservesStage(t *testing.T) {
	configuration := supervisorcore.AdmissionOptions{RunnerExecutable: "relative-runner"}
	var runtime workspaces.OperationRuntime = admissioncore.NewWorkspaceRuntime(configuration)
	configuration.RunnerExecutable = filepath.Join(t.TempDir(), "later-runner")
	if _, err := runtime.ManagedRunner(); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("runtime reread replaced configuration: %v", err)
	}
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	runner, client, err := runtime.PrepareManaged(context.Background(), store, "unused")
	var stage *admissioncore.StageError
	if runner != "" || client != nil || !errors.Is(err, pueue.ErrConfiguration) || !errors.As(err, &stage) || stage.Stage() != "resolve-runner" {
		t.Fatalf("runtime changed canonical preparation failure: %q/%v/%v", runner, client, err)
	}
}

func TestDispatchProjectionRetainsPrimaryStatusThroughEveryClosure(t *testing.T) {
	primary := errors.New("profile refused")
	restore := commonprovider.ErrAuthenticationBlocked
	observed := tasklifecycle.DispatchResult{Record: tasklifecycle.RecordAbsent, Phase: tasklifecycle.DispatchProviderSelection, Failure: primary, EnvironmentRestoreError: restore, Workspace: &workspaces.AdmissionFinish{AbortError: errors.New("abort"), DescriptionError: errors.New("describe"), CloseError: errors.New("lease close")}, StoreCloseError: errors.New("store close")}
	result := dispatchCommandResult(observed)
	if result.code != 1 || result.response.Status == preflightStatusBlocked || result.err.Error() != observed.Err().Error() {
		t.Fatalf("late closure changed primary status or error order: %+v", result)
	}
	if result.err.Error() != "profile refused\n"+restore.Error()+"\nabort\ndescribe\nlease close\nstore close" {
		t.Fatalf("closure order changed: %v", result.err)
	}
}
