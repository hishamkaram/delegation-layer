package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

const (
	managedRunnerConflictReason = "managed_workspace_runner_conflict"
	// invalidRequestMessage is the fixed public text every invalid_request shares.
	invalidRequestMessage = "Delegate could not accept the dispatch request."
	staleIdentityMessage  = "saved task identity"
)

// managedRunnerRequest is one public dispatch that selects a managed workspace
// and also names --runner. The runner is a real executable so only the
// ownership conflict, never a missing file, can refuse the request.
type managedRunnerRequest struct {
	name  string
	build func(root, cwd, brief, runner string) []string
}

func managedRunnerRequests() []managedRunnerRequest {
	isolation := func(root, cwd, brief string) []string {
		return []string{"dispatch", "--provider", "fixture:preflight", "--cwd", cwd, "--brief", brief, "--isolation", "git-worktree", "--permission", "read-only", "--budget", "1m"}
	}
	workspaceID := strings.Repeat("c", 32)
	existing := func(root, cwd, brief string) []string {
		return []string{"dispatch", "--provider", "fixture:preflight", "--brief", brief, "--workspace", workspaceID, "--permission", "read-only", "--budget", "1m"}
	}
	var requests []managedRunnerRequest
	for _, selector := range []struct {
		name string
		args func(root, cwd, brief string) []string
	}{{"isolation", isolation}, {"existing workspace", existing}} {
		requests = append(requests,
			managedRunnerRequest{name: selector.name + "/runner after command", build: func(root, cwd, brief, runner string) []string {
				return append(append([]string{"--root", root}, selector.args(root, cwd, brief)...), "--runner", runner)
			}},
			managedRunnerRequest{name: selector.name + "/runner before command", build: func(root, cwd, brief, runner string) []string {
				return append([]string{"--root", root, "--runner", runner}, selector.args(root, cwd, brief)...)
			}},
		)
	}
	return requests
}

type managedRunnerFixture struct {
	root, cwd, brief, runner string
	resources                *recordingResources
	deps                     Dependencies
}

func newManagedRunnerFixture(t *testing.T) managedRunnerFixture {
	t.Helper()
	resources := &recordingResources{forbidden: true, t: t}
	fixture, deps, _ := newEarlyResourceDispatch(t, resources)
	runner := filepath.Join(testsupport.CanonicalTempDir(t), "custom-delegate-run")
	writeRunnerFixture(t, runner, "custom runner")
	deps.Catalog = preflightFixtureCatalog(t, fixture.candidate)
	return managedRunnerFixture{
		root: fixture.root, cwd: fixture.cwd, brief: filepath.Join(fixture.cwd, "brief.md"),
		runner: runner, resources: resources, deps: deps,
	}
}

// requireNoManagedAdmission proves the refusal happened before any state,
// resource sample, or provider preparation: the state root was never created.
func (f managedRunnerFixture) requireNoManagedAdmission(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(f.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected managed runner request created or opened the state root: %v", err)
	}
	if checks, scopes := f.resources.calls(); checks != 0 || scopes != 0 {
		t.Fatalf("rejected request sampled resources: checks=%d scopes=%d", checks, scopes)
	}
}

func requireRunnerRequestFailure(t *testing.T, response Response, reason, stage, record, next string) {
	t.Helper()
	failure := response.Failure
	if failure == nil || failure.Code != "invalid_request" || failure.ReasonCode != reason || failure.Stage != stage || failure.NextAction != next || response.TaskRecord != record {
		t.Fatalf("runner request failure = record %q %+v, want invalid_request/%s at %s with %s and record %s", response.TaskRecord, failure, reason, stage, next, record)
	}
	if want := invalidRequestMessage; !strings.HasPrefix(response.Error, want) || !strings.Contains(response.Error, "omit --runner") || strings.Contains(response.Error, staleIdentityMessage) {
		t.Fatalf("runner request error is not actionable: %q", response.Error)
	}
}

func TestManagedWorkspaceRunnerConflictIsCorrectableJSON(t *testing.T) {
	for _, request := range managedRunnerRequests() {
		for _, explicitID := range []bool{false, true} {
			name := request.name + "/generated id"
			record, next := "not_created", "correct_request"
			if explicitID {
				name = request.name + "/explicit id"
				record, next = "unknown", "stop_and_report"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newManagedRunnerFixture(t)
				args := request.build(fixture.root, fixture.cwd, fixture.brief, fixture.runner)
				if explicitID {
					args = append(args, "--id", strings.Repeat("f", 32))
				}
				var stdout, stderr bytes.Buffer
				code := Run(append(args, "--json"), &stdout, &stderr, fixture.deps)
				var response Response
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatalf("exit=%d stdout=%q stderr=%q: %v", code, &stdout, &stderr, err)
				}
				if code != 2 {
					t.Fatalf("exit=%d, want 2: %s", code, &stdout)
				}
				requireRunnerRequestFailure(t, response, managedRunnerConflictReason, "prepare-workspace", record, next)
				fixture.requireNoManagedAdmission(t)
			})
		}
	}
}

func TestManagedWorkspaceRunnerConflictIsCorrectableHuman(t *testing.T) {
	for _, request := range managedRunnerRequests() {
		t.Run(request.name, func(t *testing.T) {
			fixture := newManagedRunnerFixture(t)
			var stdout, stderr bytes.Buffer
			code := Run(request.build(fixture.root, fixture.cwd, fixture.brief, fixture.runner), &stdout, &stderr, fixture.deps)
			if code != 2 {
				t.Fatalf("exit=%d, want 2: stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			if message := stderr.String(); !strings.Contains(message, invalidRequestMessage) || !strings.Contains(message, "omit --runner") || strings.Contains(message, staleIdentityMessage) || strings.Contains(message, "prepare the managed workspace") {
				t.Fatalf("human error is not actionable: %q", message)
			}
			if strings.Contains(stderr.String(), fixture.runner) {
				t.Fatal("human error exposed the runner path")
			}
			fixture.requireNoManagedAdmission(t)
		})
	}
}

func TestHelpDescribesRunnerAsAdvancedOverride(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help"}, &stdout, &stderr, Dependencies{}); code != 0 {
		t.Fatalf("help exit = %d stderr=%q", code, &stderr)
	}
	// Padding is presentation; the path type and automatic-runner guidance
	// must remain together on the actual registered runner flag.
	var runnerHelp string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--runner ABS ") {
			runnerHelp = line
			break
		}
	}
	if !strings.Contains(runnerHelp, "Advanced custom runner override; omit for the automatic runner") {
		t.Fatalf("help does not describe --runner as an advanced override:\n%s", &stdout)
	}
	for _, flag := range []string{"--root ABS ", "--pueue-config ABS "} {
		if !strings.Contains(stdout.String(), flag) {
			t.Fatalf("help lost global flag %q", flag)
		}
	}
}
