package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// The recording client wraps the canonical terminal-event fake supervisor. It
// writes the PATH each queue add was launched with beside the private
// configuration, so the probe needs no variable outside the control allowlist.
const pathRecordingSupervisorClient = `#!/bin/sh
case "${3-}" in
  add) [ "${4-}" = --help ] || printf '%s' "${PATH-unset}" > "${2%/*}/capture-add-path" ;;
esac
exec "${0%/*}/fixture-pueue" "$@"
`

// wakeEnvironmentTask records a stop before any provider launch, with a managed
// workspace use still awaiting capture. The capture helper is created but not
// submitted, so the next task observation wakes and submits it. The fake
// supervisor never runs the job, so no guardian completion races submission.
type wakeEnvironmentTask struct {
	store    *taskdir.Store
	td       *taskdir.TaskDir
	resource *workspace.Resource
	capture  string
	addPath  string
}

func newWakeEnvironmentTask(t *testing.T) wakeEnvironmentTask {
	t.Helper()
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	store, err := taskdir.InitStore(owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	resource := workspacefixture.NewResourceInStore(t, store)
	use := newManagedTaskUseInStore(t, store, resource)
	// Request a durable stop before launch; observation still has to prove closure.
	stop, err := use.td.PrepareStop(strings.Repeat("a", 32), "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = stop.Release(); err != nil {
		t.Fatal(err)
	}
	binding := bindPathRecordingSupervisor(t, owner)
	worker := executablePath(t)
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: managedTaskCaptureID,
		WorkspaceID: resource.Request().WorkspaceID, Kind: workspace.OperationCapture, Subject: use.subject,
		Deadline:         time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		WorkerExecutable: worker, ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, Supervisor: binding,
	}
	operation, err := workspace.OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	err = resource.RecordOperation(t.Context(), request)
	if err == nil {
		_, err = resource.AssociateOperation(t.Context(), workspace.CapturePurpose(use.reservation.Sequence), request)
	}
	if err = errors.Join(err, operation.Close()); err != nil {
		t.Fatal(err)
	}
	return wakeEnvironmentTask{store: store, td: use.td, resource: resource, capture: request.OperationID, addPath: filepath.Join(filepath.Dir(binding.ConfigPath), "capture-add-path")}
}

func bindPathRecordingSupervisor(t *testing.T, owner *componentSupervisorFixture) task.SupervisorRef {
	t.Helper()
	store := owner.store
	bundle := filepath.Join(owner.base, "pair")
	clientPath, daemonPath := filepath.Join(bundle, "pueue"), filepath.Join(bundle, "pueued")
	admissionfixture.WriteTerminalEventInspectionSupervisorExecutables(t, filepath.Join(bundle, "fixture-pueue"), daemonPath)
	supervisorfixture.WriteExecutable(t, clientPath, pathRecordingSupervisorClient)
	cleanup := supervisorcore.NewCleanup(admissioncore.TerminalEvidence{}, workspacescore.InspectPrivateOperation)
	options := pueue.Options{
		ObservationTimeout: inspection.AdmissionTimeout, Environment: os.Environ(), ControlOwnerExecutable: canonicalAppTestExecutable(t),
		SuccessfulCleanupAuthorizer: cleanup.AuthorizeSuccessfulCleanup, PrivateTerminalEvidencePreparer: cleanup.PrepareTerminalEvidence,
	}
	bootstrap, err := owner.bindPrivate(t.Context(), clientPath, daemonPath, options)
	if err != nil {
		t.Fatal(err)
	}
	owner.listen(t, bootstrap.Binding().Endpoint)
	control, cancelControl := supervisorcore.ControlContextWithParent(t.Context(), options)
	defer cancelControl()
	firstWait := len(owner.daemonWaits())
	client, err := supervisorcore.PreparePrivateAdmissionSupervisorContext(control, installation.ExecutableLookup{InitialExecutable: clientPath}, store, owner.setupOptions(options))
	owner.ownClient(client, err)
	owner.ownDaemonClients(client, firstWait)
	if err != nil {
		t.Fatal(err)
	}
	return client.Binding()
}

// selectedLaunchEnvironment is the caller's environment with a PATH that
// differs from the ambient process PATH, which the test replaces as well.
func selectedLaunchEnvironment(t *testing.T) (environment []string, selected, ambient string) {
	t.Helper()
	inherited := os.Getenv("PATH")
	base := supervisorfixture.CanonicalTempDir(t)
	selected = filepath.Join(base, "selected-bin") + string(os.PathListSeparator) + inherited
	ambient = filepath.Join(base, "ambient-bin") + string(os.PathListSeparator) + inherited
	environment = slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	environment = append(environment, "PATH="+selected)
	t.Setenv("PATH", ambient)
	return environment, selected, ambient
}

func (f wakeEnvironmentTask) arguments() Arguments {
	return Arguments{Root: f.store.Root, TaskID: f.td.TaskID}
}

// requireSubmitted returns the PATH the capture submission's queue add saw
// after proving the submission was recorded and no wake failure was reported.
func (f wakeEnvironmentTask) requireSubmitted(t *testing.T, stage string, view *WorkspaceResponse) string {
	t.Helper()
	recorded, err := os.ReadFile(f.addPath)
	if err != nil {
		t.Fatalf("%s did not reach the capture queue add: %v", stage, err)
	}
	operation, err := workspace.LoadOperation(t.Context(), f.store, f.capture)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := operation.Receipt(t.Context())
	if err = errors.Join(err, operation.Close()); err != nil || receipt.NumericTaskID != 9 {
		t.Fatalf("%s did not record the capture submission: %+v, %v", stage, receipt, err)
	}
	if view == nil || strings.HasPrefix(view.ReasonCode, "workspace_wake_") {
		t.Fatalf("%s reported a wake failure after submission: %+v", stage, view)
	}
	return string(recorded)
}

func (f wakeEnvironmentTask) requireNotSubmitted(t *testing.T, stage string) {
	t.Helper()
	if _, err := os.Stat(f.addPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s reached a queue add: %v", stage, err)
	}
	operation, err := workspace.LoadOperation(t.Context(), f.store, f.capture)
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.Receipt(t.Context())
	if closeErr := operation.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !workspace.OperationAbsent(err) {
		t.Fatalf("%s recorded a capture submission: %v", stage, err)
	}
}

func TestManagedTaskStatusWakeSubmitsCaptureInCallerEnvironment(t *testing.T) {
	fixture := newWakeEnvironmentTask(t)
	environment, selected, _ := selectedLaunchEnvironment(t)
	var stdout, stderr bytes.Buffer
	Run([]string{"status", fixture.td.TaskID, "--root", fixture.store.Root, "--json"}, &stdout, &stderr, Dependencies{ResourceChecker: fixtureResources, Environment: environment})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("status response %q: %v (stderr %q)", stdout.String(), err, stderr.String())
	}
	if path := fixture.requireSubmitted(t, "status", response.Workspace); path != selected {
		t.Fatalf("status capture submission PATH = %q, want selected %q", path, selected)
	}
}

func TestManagedTaskCollectWakeUsesCapturedEnvironmentCopy(t *testing.T) {
	fixture := newWakeEnvironmentTask(t)
	environment, selected, ambient := selectedLaunchEnvironment(t)
	deps := Dependencies{ResourceChecker: fixtureResources, Environment: environment}.storeDependencies()
	environment[len(environment)-1] = "PATH=" + ambient
	result := collect(fixture.arguments(), deps)
	if path := fixture.requireSubmitted(t, "collect", result.response.Workspace); path != selected {
		t.Fatalf("collect capture submission PATH = %q, want captured %q", path, selected)
	}
}

func TestManagedTaskWakeRejectsUncapturedEnvironmentBeforeSubmission(t *testing.T) {
	fixture := newWakeEnvironmentTask(t)
	selectedLaunchEnvironment(t)
	result := status(fixture.arguments(), Dependencies{ResourceChecker: fixtureResources})
	view := result.response.Workspace
	if view == nil || view.ReasonCode != "workspace_wake_evidence_fault" || view.NextAction != "stop_and_report" {
		t.Fatalf("uncaptured wake environment was not an evidence fault: %+v", view)
	}
	fixture.requireNotSubmitted(t, "uncaptured status")

	unmanaged, _ := newAppTaskInStore(t, fixture.store, strings.Repeat("9", 32), false, nil)
	t.Cleanup(func() {
		if closeErr := unmanaged.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	plain := status(Arguments{Root: fixture.store.Root, TaskID: unmanaged.TaskID}, Dependencies{ResourceChecker: fixtureResources})
	if plain.response.Workspace != nil {
		t.Fatalf("task without a managed workspace gained a workspace view: %+v", plain.response.Workspace)
	}
}

// An explicit empty scope is a captured choice: the helper is still submitted,
// and the queue add sees no inherited PATH. The fixture shell may supply its
// own default, but never the ambient process PATH.
func TestManagedTaskWakeKeepsExplicitEmptyEnvironment(t *testing.T) {
	fixture := newWakeEnvironmentTask(t)
	_, _, ambient := selectedLaunchEnvironment(t)
	result := collect(fixture.arguments(), Dependencies{ResourceChecker: fixtureResources, Environment: []string{}}.storeDependencies())
	ambientEntry, _, _ := strings.Cut(ambient, string(os.PathListSeparator))
	if path := fixture.requireSubmitted(t, "explicit empty collect", result.response.Workspace); strings.Contains(path, ambientEntry) {
		t.Fatalf("explicit empty environment fell back to the ambient PATH: %q", path)
	}
}
