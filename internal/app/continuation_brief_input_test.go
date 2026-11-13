package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestContinueCallerBriefInputIsCorrectableWithoutSuccessor(t *testing.T) {
	for _, name := range []string{"missing", "oversized"} {
		t.Run(name, func(t *testing.T) {
			fixture := newRecordedEnvironmentFixture(t, recordedEnvironmentCases()[0])
			path := filepath.Join(fixture.workspace, "private-override.md")
			if name == "oversized" {
				if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, task.MaxBriefSize+1), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, response := runBriefContinuation(t, fixture, fixture.predecessorID, path)
			if code != 2 || response.Command != "continue" || response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Code != "invalid_request" || response.Failure.Stage != "validate-request" || response.Failure.NextAction != "correct_request" {
				t.Fatalf("caller brief failure is not correctable: exit=%d response=%+v", code, response)
			}
			if strings.Contains(response.Error, path) {
				t.Fatal("public error exposed a private brief path")
			}
			fixture.assertPredecessorUnchanged(t)
			assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
			assertOnlyPredecessorTask(t, fixture)
		})
	}
}

func TestContinuePredecessorFailureIsNotCorrectableBriefInput(t *testing.T) {
	fixture := newRecordedEnvironmentFixture(t, recordedEnvironmentCases()[0])
	code, response := runBriefContinuation(t, fixture, strings.Repeat("9", 32), filepath.Join(fixture.workspace, "missing-override.md"))
	if code != 1 || response.Command != "continue" || response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Code != "continuation_unavailable" || response.Failure.Stage != "load-predecessor" || response.Failure.NextAction != "stop_and_report" {
		t.Fatalf("missing predecessor was relabeled as caller input: exit=%d response=%+v", code, response)
	}
	fixture.assertPredecessorUnchanged(t)
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
	assertOnlyPredecessorTask(t, fixture)
}

func TestContinueValidBriefOverrideAdmitsExactBytes(t *testing.T) {
	fixture := newRecordedEnvironmentFixture(t, recordedEnvironmentCases()[0])
	want := []byte("replacement\r\ntrailing space \n\n")
	path := filepath.Join(fixture.workspace, "override.md")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	code, response := runBriefContinuation(t, fixture, fixture.predecessorID, path)
	if code != 0 || response.TaskRecord != "created" || response.TaskID == "" || response.Failure != nil {
		t.Fatalf("valid brief override was not admitted: exit=%d response=%+v", code, response)
	}
	store, err := openStore(fixture.root, fixture.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	td, err := store.OpenTask(response.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	got, err := td.ReadBrief()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("admitted override changed exact bytes: %v", err)
	}
	fixture.assertPredecessorUnchanged(t)
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
}

func runBriefContinuation(t *testing.T, fixture *recordedEnvironmentFixture, predecessor, path string) (int, Response) {
	t.Helper()
	args := fixture.entryPointArguments(t, continueEntryPoint, fixture.supervisor.Binding().ConfigPath)
	for index := range args {
		if args[index] == fixture.predecessorID {
			args[index] = predecessor
		}
	}
	args = append(args, "--brief", path)
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, fixture.deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("continue exit=%d stdout=%q stderr=%q: %v", code, &stdout, &stderr, err)
	}
	return code, response
}

func assertOnlyPredecessorTask(t *testing.T, fixture *recordedEnvironmentFixture) {
	t.Helper()
	store, err := openStore(fixture.root, fixture.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ids, err := store.TaskIDs(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != fixture.predecessorID {
		t.Fatalf("failed continuation created task state: ids=%v err=%v", ids, err)
	}
}
