package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

const verificationProbeName = "verification-probe"

func TestWorkspaceVerificationUsesCapturedEnvironment(t *testing.T) {
	ambient, selected := testsupportcore.CanonicalTempDir(t), testsupportcore.CanonicalTempDir(t)
	writeVerificationProbe(t, filepath.Join(ambient, verificationProbeName), "ambient")
	writeVerificationProbe(t, filepath.Join(selected, verificationProbeName), "selected")
	t.Setenv("PATH", ambient)
	t.Setenv("VERIFICATION_MARKER", "ambient")
	t.Setenv("VERIFICATION_AMBIENT_ONLY", "leaked")
	t.Chdir(ambient)
	environment := []string{"PATH=" + selected, "VERIFICATION_MARKER=selected", "PWD=" + ambient}
	for _, argv0 := range []string{verificationProbeName, filepath.Join(selected, verificationProbeName)} {
		store, operation, workdir := newWorkspaceVerificationTestOperation(t, []string{argv0})
		candidate, err := runWorkspaceVerification(t, store, operation, environment)
		if err != nil || candidate.Status != "succeeded" || candidate.CommandExit == nil || *candidate.CommandExit != 0 {
			t.Fatalf("verification %q = %+v, %v; want success", argv0, candidate, err)
		}
		want := "selected marker=selected ambient=unset pwd=" + workdir
		if got := readVerificationStdout(t, store, operation); got != want {
			t.Fatalf("verification %q child observed %q; want %q", argv0, got, want)
		}
	}
}

func TestWorkspaceVerificationRelativeExplicitPathUsesCheckoutDirectory(t *testing.T) {
	ambient := testsupportcore.CanonicalTempDir(t)
	writeVerificationProbe(t, filepath.Join(ambient, verificationProbeName), "ambient")
	t.Chdir(ambient)
	store, operation, workdir := newWorkspaceVerificationTestOperation(t, []string{"./" + verificationProbeName})
	writeVerificationProbe(t, filepath.Join(workdir, verificationProbeName), "checkout")
	candidate, err := runWorkspaceVerification(t, store, operation, []string{})
	if err != nil || candidate.Status != "succeeded" {
		t.Fatalf("relative explicit verification = %+v, %v; want success", candidate, err)
	}
	if got, want := readVerificationStdout(t, store, operation), "checkout marker=unset ambient=unset pwd="+workdir; got != want {
		t.Fatalf("relative explicit verification observed %q; want %q", got, want)
	}
}

func TestWorkspaceVerificationMissingOrEmptyPathDoesNotInheritAmbient(t *testing.T) {
	ambient := testsupportcore.CanonicalTempDir(t)
	writeVerificationProbe(t, filepath.Join(ambient, verificationProbeName), "ambient")
	t.Setenv("PATH", ambient)
	t.Chdir(ambient)
	for _, environment := range [][]string{{}, {"PATH="}, {"VERIFICATION_MARKER=selected"}} {
		store, operation, _ := newWorkspaceVerificationTestOperation(t, []string{verificationProbeName})
		candidate, err := runWorkspaceVerification(t, store, operation, environment)
		if !errors.Is(err, exec.ErrNotFound) || candidate.CommandExit != nil || len(candidate.Artifacts) != 0 {
			t.Fatalf("verification with %q = %+v, %v; want not found", environment, candidate, err)
		}
		assertVerificationLookupError(t, err)
		assertVerificationDiagnosticsClosed(t, store, operation)
	}
}

func TestWorkspaceVerificationRelativeSearchRetainsErrDot(t *testing.T) {
	ambient := testsupportcore.CanonicalTempDir(t)
	writeVerificationProbe(t, filepath.Join(ambient, verificationProbeName), "ambient")
	t.Chdir(ambient)
	store, operation, _ := newWorkspaceVerificationTestOperation(t, []string{verificationProbeName})
	candidate, err := runWorkspaceVerification(t, store, operation, []string{"PATH=."})
	if !errors.Is(err, exec.ErrDot) || candidate.CommandExit != nil || len(candidate.Artifacts) != 0 {
		t.Fatalf("relative search verification = %+v, %v; want ErrDot", candidate, err)
	}
	assertVerificationLookupError(t, err)
	assertVerificationDiagnosticsClosed(t, store, operation)
}

func TestWorkspaceVerificationDiagnosticSetupFailurePrecedesLookup(t *testing.T) {
	for _, existing := range []string{"stdout", "stderr"} {
		store, operation, _ := newWorkspaceVerificationTestOperation(t, []string{verificationProbeName})
		scratch := workspace.ScratchPath(store.Root, operation.Request().OperationID)
		if err := os.MkdirAll(scratch, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scratch, existing), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		candidate, err := runWorkspaceVerification(t, store, operation, []string{})
		if !errors.Is(err, os.ErrExist) || errors.Is(err, exec.ErrNotFound) || candidate.CommandExit != nil {
			t.Fatalf("existing %s diagnostic = %+v, %v; want setup failure before lookup", existing, candidate, err)
		}
	}
}

func TestWorkspaceVerificationSelectedErrDotOptOutDoesNotSearchAmbientPath(t *testing.T) {
	ambient := testsupportcore.CanonicalTempDir(t)
	writeVerificationProbe(t, filepath.Join(ambient, verificationProbeName), "ambient")
	t.Setenv("PATH", ambient)
	t.Chdir(ambient)
	store, operation, _ := newWorkspaceVerificationTestOperation(t, []string{verificationProbeName})
	candidate, err := runWorkspaceVerification(t, store, operation, []string{"PATH=.", "GODEBUG=execerrdot=0"})
	if !errors.Is(err, os.ErrNotExist) || candidate.CommandExit != nil {
		t.Fatalf("opt-out verification = %+v, %v; want the checkout-relative launch to be missing", candidate, err)
	}
}

// assertVerificationLookupError requires the native lookup error for the
// requested program name, as exec.Command reports it from Start.
func assertVerificationLookupError(t *testing.T, err error) {
	t.Helper()
	var lookupErr *exec.Error
	if !errors.As(err, &lookupErr) || lookupErr.Name != verificationProbeName {
		t.Fatalf("verification lost the native lookup error: %v", err)
	}
}

// assertVerificationDiagnosticsClosed requires both empty diagnostics to exist
// and, where the host exposes its descriptor table, no open descriptor to them.
func assertVerificationDiagnosticsClosed(t *testing.T, store *taskdir.Store, operation *workspace.Operation) {
	t.Helper()
	scratch := workspace.ScratchPath(store.Root, operation.Request().OperationID)
	diagnostics := make(map[string]bool)
	for _, name := range []string{"stdout", "stderr"} {
		path, err := filepath.EvalSymlinks(filepath.Join(scratch, name))
		if err != nil {
			t.Fatalf("verification lost its %s diagnostic: %v", name, err)
		}
		if info, statErr := os.Lstat(path); statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatalf("%s diagnostic = %v, %v; want an empty regular file", name, info, statErr)
		}
		diagnostics[path] = true
	}
	descriptors, err := os.ReadDir("/proc/self/fd")
	if errors.Is(err, os.ErrNotExist) {
		return // This host does not expose descriptor targets; closure stays covered on Linux.
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range descriptors {
		target, readErr := os.Readlink(filepath.Join("/proc/self/fd", descriptor.Name()))
		if errors.Is(readErr, os.ErrNotExist) {
			continue // The descriptor closed after the directory was listed.
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if diagnostics[target] {
			t.Fatalf("verification left diagnostic %s open", target)
		}
	}
}

func runWorkspaceVerification(t *testing.T, store *taskdir.Store, operation *workspace.Operation, environment []string) (workspace.OperationResult, error) {
	t.Helper()
	var candidate workspace.OperationResult
	var actionErr error
	workErr, stopErr := execution.RunSupervised(operation.Deadline(), execution.SupervisedOptions{Stopper: workspaceSignalTestStopper{}}, func(scope execution.PreflightScope) error {
		candidate, actionErr = runWorkspaceAction(scope, store, operation, environment)
		return nil
	})
	if workErr != nil || stopErr != nil {
		t.Fatalf("verification supervision failed: %v %v", workErr, stopErr)
	}
	return candidate, actionErr
}

func readVerificationStdout(t *testing.T, store *taskdir.Store, operation *workspace.Operation) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(workspace.ScratchPath(store.Root, operation.Request().OperationID), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// writeVerificationProbe reports which executable ran and the child's
// environment; it uses only shell builtins so an empty PATH still runs it.
func writeVerificationProbe(t *testing.T, path, label string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s marker=%s ambient=%s pwd=%s' '" + label + "' \"${VERIFICATION_MARKER-unset}\" \"${VERIFICATION_AMBIENT_ONLY-unset}\" \"$PWD\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func newWorkspaceVerificationTestOperation(t *testing.T, arguments []string) (*taskdir.Store, *workspace.Operation, string) {
	t.Helper()
	resource, workdir := newMappedWorkspaceExecutionFixture(t)
	store, err := openStore(resource.StateRoot(), Dependencies{ResourceChecker: fixtureResources}.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	digest := task.ComputeSHA256([]byte("environment verification"))
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: resource.Request().WorkspaceID,
		OperationID: strings.Repeat("c", 32), Kind: workspace.OperationVerify,
		Subject:  workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: digest},
		Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: digest,
		Supervisor: testsupport.ContinuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
		Arguments:  arguments,
	}
	operation, err := workspace.OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store, operation, workdir
}
