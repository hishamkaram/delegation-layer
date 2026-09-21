package app

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type autoTestInterpreter struct{ reference task.PredicateRef }

func (i autoTestInterpreter) Reference() task.PredicateRef { return i.reference }

func (autoTestInterpreter) Evaluate(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "test"}, nil
}

func TestSelectAutoProviderUsesStaticReadinessAndDefaultPermission(t *testing.T) {
	workspace := filepath.Clean(t.TempDir())
	root := filepath.Clean(t.TempDir())
	runtimeRoot := filepath.Clean(t.TempDir())
	makeRegistration := func(id string, prepare commonprovider.PrepareCandidate) commonprovider.Registration {
		ref := task.PredicateRef{Adapter: id, Mode: config.ModeReadOnly, Version: "1", SHA256: task.ComputeSHA256([]byte(id))}
		return commonprovider.Registration{
			Description: commonprovider.Description{
				ID:               id,
				SupportedModes:   []string{config.ModeReadOnly},
				SupportedOptions: []string{commonprovider.OptionContinuation},
				Continuation:     commonprovider.ContinuationNative,
				Runtime:          commonprovider.RuntimeCapability{RequiredFlags: []string{"--test"}},
				Discoverable:     true,
			},
			Prepare:      prepare,
			Interpreters: []predicate.Interpreter{autoTestInterpreter{reference: ref}},
		}
	}
	var calls []string
	prepare := func(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		calls = append(calls, request.Provider)
		candidate := commonprovider.ProfileCandidate{
			Directory:     request.CanonicalCwd,
			WritableRoots: []string{filepath.Join(runtimeRoot, "provider-runtime")},
			Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
				if request.Provider == "alpha:run" {
					return commonprovider.PreparedProfile{}, errors.New("alpha unavailable")
				}
				return commonprovider.PreparedProfile{
					Plan:            execution.Plan{Executable: "/bin/provider", Directory: request.CanonicalCwd, Predicate: task.PredicateRef{Adapter: request.Provider, Mode: request.Mode, Version: "1", SHA256: task.ComputeSHA256([]byte(request.Provider))}},
					ObservedVersion: "test-v1",
					Effective:       task.EffectiveConfig{Containment: "read-only", Approval: "test", Digest: task.ComputeSHA256([]byte("test-config"))},
					WritableRoots:   []string{filepath.Join(runtimeRoot, "provider-runtime")},
				}, nil
			},
		}
		return candidate, nil
	}
	catalog, err := commonprovider.NewCatalog(
		makeRegistration("zulu:run", prepare),
		makeRegistration("alpha:run", prepare),
	)
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{Catalog: catalog}.normalized()
	provider, err := selectAutoProvider(deps, root, strings.Repeat("a", 32), strings.Repeat("b", 32), workspace, task.TaskConfig{Budget: "1m"}, []byte("brief"))
	if err != nil || provider != "zulu:run" {
		t.Fatalf("auto provider=%q err=%v calls=%v", provider, err, calls)
	}
	if !strings.EqualFold(strings.Join(calls, ","), "alpha:run,zulu:run") {
		t.Fatalf("providers were not tried in deterministic order: %v", calls)
	}
}

func TestTimeoutContinuationRequiresDurableTermination(t *testing.T) {
	request := &task.StopRequestRecord{Cause: "budget"}
	response := newResponse("status")
	records := []taskdir.StopRecord{{Request: request}}
	applyTimeoutContinuation(&response, "/tmp/state", &task.TaskRecord{TaskID: strings.Repeat("a", 32), Provider: "pi:json"}, records, NativeCatalog(), nil, "", nil)
	if response.Status != "" || response.Continuation != nil {
		t.Fatalf("unobserved budget stop was treated as timeout: %+v", response)
	}
	records[0].Observation = &task.StopObservedRecord{Terminated: true}
	applyTimeoutContinuationWithRunnerState(&response, "/tmp/state", &task.TaskRecord{TaskID: strings.Repeat("a", 32), Provider: "pi:json"}, records, NativeCatalog(), &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"}, "/usr/bin/delegate-run", nil, true, true)
	if response.Status != "timed_out" || response.Continuation == nil || !response.Continuation.Resumable {
		t.Fatalf("terminated budget stop was not resumable: %+v", response)
	}
}

func TestTimeoutContinuationWaitsForRunnerLeaseRelease(t *testing.T) {
	taskID := strings.Repeat("d", 32)
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	request := &task.TaskRecord{TaskID: taskID, Provider: "pi:json"}
	session := &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"}
	response := newResponse("status")
	applyTimeoutContinuationWithRunnerState(&response, "/tmp/state", request, records, NativeCatalog(), session, "/usr/bin/delegate-run", nil, false, true)
	if response.Status != "timed_out" || response.Continuation == nil || response.Continuation.Resumable || response.Continuation.ContinueCommand != "" {
		t.Fatalf("active runner lease was advertised as resumable: %+v", response.Continuation)
	}
	if response.Continuation.Reason != "runner lease is still active" {
		t.Fatalf("active runner lease reason=%q", response.Continuation.Reason)
	}
}

func TestTimeoutContinuationWithoutRecordedRunnerOmitsCommand(t *testing.T) {
	taskID := strings.Repeat("e", 32)
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	response := newResponse("status")
	applyTimeoutContinuationWithRunnerState(&response, "/tmp/state", &task.TaskRecord{TaskID: taskID, Provider: "pi:json"}, records, NativeCatalog(), &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"}, "", nil, true, true)
	if response.Continuation == nil || response.Continuation.Resumable || response.Continuation.ContinueCommand != "" {
		t.Fatalf("legacy task advertised an unusable continuation: %+v", response.Continuation)
	}
	if response.Continuation.Reason != "runner executable is unavailable for this task" {
		t.Fatalf("missing runner reason=%q", response.Continuation.Reason)
	}
}

func TestTimeoutContinuationWithoutRecordedEnvironmentOmitsCommand(t *testing.T) {
	taskID := strings.Repeat("f", 32)
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	response := newResponse("status")
	applyTimeoutContinuationWithRunnerState(&response, "/tmp/state", &task.TaskRecord{TaskID: taskID, Provider: "pi:json"}, records, NativeCatalog(), &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"}, "/usr/bin/delegate-run", nil, true, false)
	if response.Continuation == nil || response.Continuation.Resumable || response.Continuation.ContinueCommand != "" {
		t.Fatalf("legacy task advertised an unusable continuation: %+v", response.Continuation)
	}
	if response.Continuation.Reason != "launch environment is unavailable for this task" {
		t.Fatalf("missing environment reason=%q", response.Continuation.Reason)
	}
}

func TestTimeoutContinuationSurfacesInvalidProviderIdentity(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	if err := os.WriteFile(filepath.Join(td.Dir, "provider.ref.json"), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	response := newResponse("status")
	if err = applyTimeoutContinuationWithRecords(&response, store.Root, td, req, meta, records, NativeCatalog()); err == nil {
		t.Fatal("invalid provider identity was hidden")
	}
}

func TestTimeoutContinuationFromTaskSurfacesMalformedStopEvidence(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	stopDir := filepath.Join(td.Dir, "stop")
	if err := os.MkdirAll(stopDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stopDir, "bad.request.json"), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	response := newResponse("status")
	if err = applyTimeoutContinuationFromTask(&response, store.Root, td, req, meta, NativeCatalog()); err == nil {
		t.Fatal("malformed timeout evidence was hidden")
	}
}

func TestContinuationCommandPreservesCustomRunner(t *testing.T) {
	taskID := strings.Repeat("a", 32)
	got := continuationCommand("/tmp/state", taskID, "/tmp/custom runner", nil)
	want := "delegate --root '/tmp/state' --runner '/tmp/custom runner' continue --task " + taskID + " --json"
	if got != want {
		t.Fatalf("continuation command = %q, want %q", got, want)
	}
}

func TestContinuationCommandPreservesExternalSupervisor(t *testing.T) {
	taskID := strings.Repeat("b", 32)
	supervisor := &task.SupervisorRef{ConfigPath: "/tmp/external pueue.yml"}
	got := continuationCommand("/tmp/state", taskID, "", supervisor)
	want := "delegate --root '/tmp/state' --pueue-config '/tmp/external pueue.yml' continue --task " + taskID + " --json"
	if got != want {
		t.Fatalf("continuation command = %q, want %q", got, want)
	}
}

func TestNonresumableTimeoutOmitsContinuationCommand(t *testing.T) {
	taskID := strings.Repeat("c", 32)
	response := newResponse("status")
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	applyTimeoutContinuation(&response, "/tmp/state", &task.TaskRecord{TaskID: taskID, Provider: config.ProviderFixture}, records, NativeCatalog(), &task.ProviderRefRecord{Provider: config.ProviderFixture, ConversationID: "session"}, "", nil)
	if response.Continuation == nil || response.Continuation.Resumable || response.Continuation.ContinueCommand != "" {
		t.Fatalf("nonresumable timeout advertised a command: %+v", response.Continuation)
	}
}

func TestCollectReplaysCommittedWinnerBeforeMalformedTimeoutMetadata(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing terminal winner cleanup=%v collect=%v", cleanupErr, collectErr)
	}
	stopDir := filepath.Join(td.Dir, "stop")
	if err := os.MkdirAll(stopDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stopDir, "bad.request.json"), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal([]byte(stdout.String()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Outcome == nil || response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("committed winner was hidden by timeout metadata: code=%d response=%+v stderr=%q", code, response, stderr.String())
	}
}

func TestCollectDecoratesAlreadyPublishedOutcomeWithTimeoutContinuation(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	prepareAppPublishedTimeoutEvidence(t, td)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing terminal winner cleanup=%v collect=%v", cleanupErr, collectErr)
	}

	var stdout, stderr strings.Builder
	code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal([]byte(stdout.String()), &response); err != nil {
		t.Fatal(err)
	}
	if code != 0 || response.Publication != task.PublicationCommitted.String() || response.Status != "timed_out" || response.Continuation == nil {
		t.Fatalf("published timeout metadata was omitted: code=%d response=%+v stderr=%q", code, response, stderr.String())
	}
}

func prepareAppPublishedTimeoutEvidence(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	submission, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.Release(); err != nil {
		t.Fatal(err)
	}
	submit, err := td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	ref := submit.Supervisor
	if err = td.RecordSupervisorReceipt(task.SupervisorReceipt{
		SchemaVersion:        task.SchemaVersion,
		RootID:               submit.RootID,
		TaskID:               submit.TaskID,
		SpecSHA256:           submit.SpecSHA256,
		MetaSHA256:           submit.MetaSHA256,
		NumericTaskID:        7,
		Label:                submit.Label,
		ConfigPath:           ref.ConfigPath,
		ConfigDigest:         ref.ConfigDigest,
		Endpoint:             ref.Endpoint,
		ObservedVersion:      ref.ObservedVersion,
		ClientExecutable:     ref.ClientExecutable,
		ClientSHA256:         ref.ClientSHA256,
		ResolvedConfigSHA256: ref.ResolvedConfigSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	stopID := "budget"
	stop, err := td.PrepareStop(stopID, "budget", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = stop.Release(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStopReply(stopID, task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true, Message: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStopObservation(stopID, task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}); err != nil {
		t.Fatal(err)
	}
}
