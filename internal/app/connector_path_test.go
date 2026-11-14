package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newPathConnectorFixture(t *testing.T) *connectorFixture {
	t.Helper()
	return newPathConnectorFixtureWithMetadata(t, nil)
}

func newPathConnectorFixtureWithMetadata(t *testing.T, configure func(*task.MetaRecord, *Dependencies)) *connectorFixture {
	t.Helper()
	f := newConnectorFixture(t)
	if err := f.store.EnableWorkspaceConnectorContext(t.Context(), true, true); err != nil {
		t.Fatal(err)
	}
	project, err := taskdir.ObservePathProjectContext(t.Context(), f.request.CanonicalCwd)
	if err != nil {
		t.Fatal(err)
	}
	token, err := taskdir.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	_, meta, err := f.task.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *f.request
	request.TaskID, meta.TaskID = strings.Repeat("e", 32), strings.Repeat("e", 32)
	request.ConnectorContext = &task.ConnectorContext{
		RootToken: f.rootToken, Project: project,
		WorkspaceConstraint: &task.WorkspaceConstraint{Token: token, RelativeProject: "."},
	}
	if configure != nil {
		configure(meta, &f.deps)
	}
	td, err := f.store.CreateTask(request.TaskID, &request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	f.task, f.request, f.projectToken = td, &request, token
	return f
}

func pathConnectorArguments(f *connectorFixture) Arguments {
	return Arguments{
		Root: f.store.Root, Cwd: f.request.CanonicalCwd,
		Connector: ConnectorArguments{
			ExpectedRoot: f.rootToken, ExpectedProject: f.projectToken,
			ExpectedWorkspace: f.request.ConnectorContext.WorkspaceConstraint.Token, RelativeProject: ".",
			origin: f.origin.Clone(),
		},
	}
}

func replaceConnectorProject(t *testing.T, f *connectorFixture) {
	t.Helper()
	if err := os.Rename(f.request.CanonicalCwd, f.request.CanonicalCwd+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.request.CanonicalCwd, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestConnectorPathGuardPrepareAfterReplacement(t *testing.T) {
	f := newPathConnectorFixture(t)
	commitConnectorFixtureResult(t, f, "accepted answer")
	_, _, spec, _, err := f.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(f.task.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	replaceConnectorProject(t, f)
	a := pathConnectorArguments(f)
	for range 2 {
		if err = prepareGuardedDispatchContext(t.Context(), &a, f.deps); err != nil {
			t.Fatalf("same canonical path failed guarded preparation: %v", err)
		}
		if a.Cwd != f.request.CanonicalCwd || !reflect.DeepEqual(a.Connector.binding, f.request.ConnectorContext) {
			t.Fatalf("preparation changed the saved logical binding: %+v", a.Connector.binding)
		}
	}
	response, code := f.public(t, "resolve", f.request.TaskID, "--expected-spec-sha256", spec, "--expected-workspace", f.projectToken, "--relative-project", ".", "--include-workspace-constraint")
	if code != 0 {
		t.Fatalf("path-bound preparation failed: code=%d %+v", code, response)
	}
	requirePathConnectorReceipt(t, f, response, spec)
	after, err := os.ReadFile(filepath.Join(f.task.Dir, "task.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("preparation changed the saved request: %v", err)
	}
	if _, err = f.task.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal replay submitted new work: %v", err)
	}
}

func requirePathConnectorReceipt(t *testing.T, f *connectorFixture, response Response, spec string) {
	t.Helper()
	if response.Connector == nil || response.Connector.Receipt == nil {
		t.Fatalf("path-bound preparation lost receipt: %+v", response)
	}
	receipt := response.Connector.Receipt
	if receipt.SpecSHA256 != spec || receipt.ProjectToken != f.projectToken || receipt.Origin != f.origin {
		t.Fatalf("path-bound preparation changed immutable receipt: %+v", receipt)
	}
}

func TestConnectorPathResumeReachesSavedProfileAfterReplacement(t *testing.T) {
	profileRefusal := errors.New("bounded profile refusal after successful path guard")
	profileCalls := 0
	f := newPathConnectorFixtureWithMetadata(t, func(meta *task.MetaRecord, deps *Dependencies) {
		supervisor, options, _, _ := newOrdinarySubmissionReceiptSupervisor(t)
		t.Cleanup(func() {
			if err := supervisor.Close(); err != nil {
				t.Error(err)
			}
		})
		meta.SupervisorConfig = supervisor.Binding()
		meta.RunnerExecutable, meta.RunnerOwnership = canonicalAppTestExecutable(t), task.RunnerOwnershipCustom
		meta.Environment, meta.EnvironmentRecorded = options.Environment, true
		deps.InitialSupervisorExecutable, deps.SupervisorOptions, deps.Environment = supervisor.Binding().ClientExecutable, options, options.Environment
		deps.PrepareProfile = func(request task.TaskRecord) (PreparedProfile, error) {
			profileCalls++
			if request.Origin == nil || request.ConnectorContext == nil || !request.ConnectorContext.Project.IsPath() {
				t.Error("resume did not prepare the exact saved path request")
			}
			return PreparedProfile{}, profileRefusal
		}
	})
	_, _, spec, _, err := f.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	replaceConnectorProject(t, f)
	response, code := f.public(t, "resolve", f.request.TaskID, "--resume-admission", "--expected-spec-sha256", spec, "--expected-workspace", f.projectToken, "--relative-project", ".")
	if code != 2 || profileCalls != 1 || !strings.Contains(response.Error, profileRefusal.Error()) {
		t.Fatalf("path resume failed before saved-profile preparation: code=%d calls=%d %+v", code, profileCalls, response)
	}
	if _, err = f.task.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused profile submitted work: %v", err)
	}
}

func TestConnectorPathGuardRejectsDifferentRequestedDirectory(t *testing.T) {
	f := newPathConnectorFixture(t)
	a := pathConnectorArguments(f)
	a.Cwd = t.TempDir()
	if err := prepareGuardedDispatchContext(t.Context(), &a, f.deps); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("different requested canonical path accepted: %v", err)
	}
}

func TestConnectorPathResumeKeepsExactOriginAndSpec(t *testing.T) {
	f := newPathConnectorFixture(t)
	commitConnectorFixtureResult(t, f, "answer")
	_, _, spec, _, err := f.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	replaceConnectorProject(t, f)
	for _, field := range []string{"origin", "spec", "workspace"} {
		t.Run(field, func(t *testing.T) {
			requested := *f
			expectedSpec, workspace := spec, f.projectToken
			switch field {
			case "origin":
				requested.origin.OperationID = "other-operation"
			case "spec":
				expectedSpec = strings.Repeat("f", 64)
			case "workspace":
				workspace = ""
			}
			args := []string{"resolve", f.request.TaskID, "--resume-admission", "--expected-spec-sha256", expectedSpec}
			if workspace != "" {
				args = append(args, "--expected-workspace", workspace, "--relative-project", ".")
			}
			response, code := requested.public(t, args...)
			if code == 0 || response.Connector == nil || response.Connector.Receipt != nil {
				t.Fatalf("resume accepted changed %s: code=%d %+v", field, code, response)
			}
		})
	}
}

func TestConnectorPathLocalSuccessorIsExcludedFromGatewayControl(t *testing.T) {
	f := newPathConnectorFixture(t)
	_, meta, err := f.task.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	local := *f.request
	local.TaskID, meta.TaskID, local.Origin = strings.Repeat("f", 32), strings.Repeat("f", 32), nil
	td, err := f.store.CreateTask(local.TaskID, &local, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	response, code := f.public(t, "resolve", local.TaskID, "--include-workspace-constraint")
	if code == 0 || response.Connector == nil || response.Connector.Receipt != nil {
		t.Fatalf("local successor became a Gateway task: code=%d %+v", code, response)
	}
	inventory, err := f.store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, f.rootToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range inventory.Entries {
		if entry.TaskID == local.TaskID {
			t.Fatal("Gateway inventory included a local path-bound successor")
		}
	}
}
