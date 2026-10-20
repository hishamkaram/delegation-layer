package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/pi"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// These tests drive the real Pi consumers (identity observer wiring, the
// execution runner, task directory sealing and the public recheck command)
// with synthetic JSON streams. The streams intentionally include orderings
// that isolate one consumer; they make no claim about native Pi ordering.

const (
	piConsumerSession = "123e4567-e89b-12d3-a456-426614174000"
	piConsumerAnswer  = "pi consumer answer"
)

var piConsumerModes = []string{pi.ModeReadOnly, pi.ModeWorkspaceWrite}

func piConsumerMessage(text string) string {
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":%q}],"stopReason":"stop"}`, text)
}

func piConsumerSessionLine(id string) string {
	return fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":"/workspace"}`, id)
}

// piConsumerCycle is one complete successful agent cycle.
func piConsumerCycle(answer string) []string {
	message := piConsumerMessage(answer)
	return []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, message),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, message),
		fmt.Sprintf(`{"type":"agent_end","willRetry":false,"messages":[%s]}`, message),
	}
}

// piConsumerRetryCycles is a failed cycle, a native retry, and a final cycle
// whose tool turn carries the supplied auto_retry_end payload. The existing
// retry ordering is preserved so evaluation reaches the failure policy.
func piConsumerRetryCycles(answer, retryEnd string) []string {
	failed := `{"role":"assistant","content":[{"type":"thinking"}],"stopReason":"error","errorMessage":"retryable provider error"}`
	tool := `{"role":"assistant","content":[{"type":"toolCall","id":"call-retry","name":"read","arguments":{}}],"stopReason":"toolUse"}`
	final := piConsumerMessage(answer)
	return []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, failed),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, failed),
		fmt.Sprintf(`{"type":"agent_end","willRetry":true,"messages":[%s]}`, failed),
		`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"retryable provider error"}`,
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, tool),
		retryEnd,
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[{}]}`, tool),
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, final),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, final),
		fmt.Sprintf(`{"type":"agent_end","willRetry":false,"messages":[%s,%s]}`, tool, final),
	}
}

func piConsumerStream(lines ...[]string) string {
	var all []string
	for _, group := range lines {
		all = append(all, group...)
	}
	return strings.Join(all, "\n") + "\n"
}

// piConsumerExecution is one admitted Pi task whose brief is the synthetic
// provider stream. A finite fixture executable named pi echoes its stdin, so
// the execution runner captures exactly that stream as provider stdout.
type piConsumerExecution struct {
	td      *taskdir.TaskDir
	request task.TaskRecord
	profile commonprovider.PreparedProfile
}

const (
	piConsumerAgentDirEnv   = "PI_CODING_AGENT_DIR"
	piConsumerSessionDirEnv = "PI_CODING_AGENT_SESSION_DIR"
)

// isolatePiConsumerEnvironment points every input real Pi candidate
// preparation consumes (HOME, PATH and both native directory selectors) at
// fixture-owned directories, so no outer selector or user configuration is
// read. The two directories are distinct and exist, as the provider requires.
func isolatePiConsumerEnvironment(t *testing.T, base, cliDir string) (agentDir, sessionDir string) {
	t.Helper()
	home := filepath.Join(base, "home")
	agentDir, sessionDir = filepath.Join(base, "pi-agent"), filepath.Join(base, "pi-sessions")
	for _, directory := range []string{home, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", cliDir)
	t.Setenv(piConsumerAgentDirEnv, agentDir)
	t.Setenv(piConsumerSessionDirEnv, sessionDir)
	return agentDir, sessionDir
}

func newPiConsumerExecution(t *testing.T, mode, stream string) piConsumerExecution {
	t.Helper()
	base := testsupportcore.CanonicalTempDir(t)
	cliDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(cliDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRunnerFixture(t, filepath.Join(cliDir, "pi"), "#!/bin/sh\nexec /bin/cat\n")
	agentDir, sessionDir := isolatePiConsumerEnvironment(t, base, cliDir)
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := NativeCatalog()
	store, err := taskdir.InitStoreWithPredicates(filepath.Join(base, "state"), catalog.Registry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	request, err := admission.BuildRequest(store.RootID, strings.Repeat("c", 32), pi.Provider, workspace, task.TaskConfig{Permission: mode, Budget: "1m0s"}, []byte(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.Candidate(request)
	if err != nil {
		t.Fatal(err)
	}
	// SYNTHETIC inspection facts: they bind the fixture executable to the
	// candidate and make no claim of a runtime preflight or native Pi proof.
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "synthetic-fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := admission.FinalizeCandidate(candidate, request, facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{piConsumerAgentDirEnv + "=" + agentDir, piConsumerSessionDirEnv + "=" + sessionDir} {
		if !slices.Contains(profile.Plan.Environment, selector) {
			t.Fatalf("launch environment does not carry the fixture-owned selector %q", selector)
		}
	}
	supervisorRef := testsupport.ContinuationSupervisorRef(store.Root, strings.Repeat("a", 64), "unix:"+filepath.Join(base, "supervisor.sock"))
	meta := tasklifecycle.NewMeta(request, profile, supervisorRef, "", task.RunnerOwnershipManaged, "app-test")
	td, err := store.CreateTask(request.TaskID, &request, []byte(stream), &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return piConsumerExecution{td: td, request: request, profile: profile}
}

// run executes the finite provider through the production identity wiring and
// the real execution runner, which owns capture, Wait, and permit cleanup.
func (e piConsumerExecution) run(t *testing.T) execution.Result {
	t.Helper()
	permit, err := e.td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := makeIdentityObserver(e.profile, e.request, e.td)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := observer.(execution.RecordIdentityObserver); !ok {
		t.Fatalf("Pi identity observer %T is not record aware", observer)
	}
	result := execution.Run(e.td, permit, e.profile.Plan, execution.Options{Identity: observer})
	if joined := errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError); joined != nil {
		t.Fatal(joined)
	}
	if result.Outcome == nil {
		t.Fatal("execution produced no outcome")
	}
	return result
}

func TestPiConsumerRecordIdentity(t *testing.T) {
	duplicateSessionID := strings.Replace(piConsumerSessionLine(piConsumerSession), `"id":"`+piConsumerSession+`"`, `"id":"`+piConsumerSession+`","id":"223e4567-e89b-12d3-a456-426614174000"`, 1)
	cases := []struct {
		name         string
		stream       string
		wantVerdict  string
		wantIdentity bool
	}{
		{
			name:         "valid complete stream",
			stream:       piConsumerStream([]string{piConsumerSessionLine(piConsumerSession)}, piConsumerCycle(piConsumerAnswer), []string{`{"type":"agent_settled","aborted":false}`}),
			wantVerdict:  task.VerdictCommitted,
			wantIdentity: true,
		},
		{
			name:         "duplicate aborted before the sole session header",
			stream:       piConsumerStream([]string{`{"type":"agent_settled","aborted":false,"aborted":true}`, piConsumerSessionLine(piConsumerSession)}, piConsumerCycle(piConsumerAnswer)),
			wantVerdict:  task.VerdictRejected,
			wantIdentity: true,
		},
		{
			name:         "duplicate retry success before the sole session header",
			stream:       piConsumerStream([]string{`{"type":"auto_retry_end","success":true,"success":false}`, piConsumerSessionLine(piConsumerSession)}, piConsumerCycle(piConsumerAnswer)),
			wantVerdict:  task.VerdictRejected,
			wantIdentity: true,
		},
		{
			name:        "duplicate session id",
			stream:      piConsumerStream([]string{duplicateSessionID}, piConsumerCycle(piConsumerAnswer)),
			wantVerdict: task.VerdictRejected,
		},
	}
	for _, mode := range piConsumerModes {
		for _, testCase := range cases {
			t.Run(mode+"/"+testCase.name, func(t *testing.T) {
				// Conflicting outer selectors must never reach candidate preparation.
				t.Setenv(piConsumerAgentDirEnv, "/tmp")
				t.Setenv(piConsumerSessionDirEnv, "/var/tmp")
				fixture := newPiConsumerExecution(t, mode, testCase.stream)
				result := fixture.run(t)
				if result.Outcome.Verdict != testCase.wantVerdict {
					t.Fatalf("verdict=%q want %q outcome=%+v", result.Outcome.Verdict, testCase.wantVerdict, result.Outcome)
				}
				assertPiConsumerAnswer(t, fixture.td, testCase.wantVerdict == task.VerdictCommitted)
				assertPiConsumerIdentity(t, fixture.td, testCase.wantIdentity)
			})
		}
	}
}

func assertPiConsumerAnswer(t *testing.T, td *taskdir.TaskDir, committed bool) {
	t.Helper()
	answer, err := os.ReadFile(filepath.Join(td.Dir, "result.txt"))
	if committed {
		if err != nil || string(answer) != piConsumerAnswer {
			t.Fatalf("committed answer=%q err=%v", answer, err)
		}
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected task published an answer: %q %v", answer, err)
	}
}

func assertPiConsumerIdentity(t *testing.T, td *taskdir.TaskDir, want bool) {
	t.Helper()
	identity, absent, err := td.ReadProviderIdentityPresence()
	if !want {
		// Absence is reported with the underlying not-exist error.
		if !absent || !task.RecordAbsent(err) {
			t.Fatalf("identity was recorded for an ambiguous session header: absent=%t identity=%+v err=%v", absent, identity, err)
		}
		return
	}
	if err != nil || absent || identity == nil || identity.Provider != pi.Provider || identity.ConversationID != piConsumerSession {
		t.Fatalf("exact identity was not recorded: absent=%t identity=%+v err=%v", absent, identity, err)
	}
}

// piConsumerRecheckSource resolves the exact prior fresh-default revision of
// mode (read-only v6, workspace-write v8) from Pi's own registration.
func piConsumerRecheckSource(t *testing.T, mode, version string) task.PredicateRef {
	t.Helper()
	for _, interpreter := range pi.Registration().Interpreters {
		if reference := interpreter.Reference(); reference.Mode == mode && reference.Version == version {
			return reference
		}
	}
	t.Fatalf("Pi %s v%s is not registered", mode, version)
	return task.PredicateRef{}
}

// piConsumerCloser registers close for test cleanup at acquisition and returns
// an idempotent early closer, so an abnormal exit (Fatal, Skip) still releases
// the handle exactly once while a recheck can still run after an early close.
func piConsumerCloser(t *testing.T, name string, handle interface{ Close() error }) func() {
	t.Helper()
	var once sync.Once
	closeHandle := func() {
		once.Do(func() {
			if err := handle.Close(); err != nil {
				t.Errorf("close %s: %v", name, err)
			}
		})
	}
	t.Cleanup(closeHandle)
	return closeHandle
}

// acquirePiRecheckTask creates the historical task in a store rooted at
// stateRoot. Each handle's closer is registered the moment it is acquired;
// cleanups run in reverse, so the task closes before its store. The returned
// release closes both early, in that order.
func acquirePiRecheckTask(t *testing.T, registry predicate.Registry, source task.PredicateRef, stateRoot string) (store *taskdir.Store, td *taskdir.TaskDir, release func()) {
	t.Helper()
	store, err := taskdir.InitStoreWithPredicates(stateRoot, registry)
	if err != nil {
		t.Fatal(err)
	}
	closeStore := piConsumerCloser(t, "store", store)
	td = createProviderRecheckTask(t, store, strings.Repeat("f", 32), providerRecheckSource{provider: pi.Provider, reference: source})
	closeTask := piConsumerCloser(t, "task", td)
	return store, td, func() { closeTask(); closeStore() }
}

func sealedPiRecheckTask(t *testing.T, registry predicate.Registry, source task.PredicateRef, stdout string) (root, taskID string, outcome *task.OutcomeRecord) {
	t.Helper()
	store, td, release := acquirePiRecheckTask(t, registry, source, filepath.Join(t.TempDir(), "state"))
	outcome = sealOutputUpgradeTask(t, td, pi.Provider, piConsumerSession, source, stdout)
	root, taskID = store.Root, td.TaskID
	release()
	return root, taskID, outcome
}

// TestPiConsumerRecheckFixtureReleasesLeaseAfterSealFailure injects a failure
// after the start lease is taken, exactly where a Fatal inside sealing would
// leave the lease held. SkipNow leaves the subtest through the same Goexit
// path as Fatal, so only the cleanups registered at acquisition can release
// the lease. A second handle proves the lease is observably held first.
func TestPiConsumerRecheckFixtureReleasesLeaseAfterSealFailure(t *testing.T) {
	registry := NativeCatalog().Registry()
	source := piConsumerRecheckSource(t, pi.ModeReadOnly, "6")
	stateRoot := filepath.Join(t.TempDir(), "state")
	t.Run("seal fails after the start lease", func(t *testing.T) {
		_, td, _ := acquirePiRecheckTask(t, registry, source, stateRoot)
		permit, err := td.PrepareStart(0)
		if err != nil {
			t.Fatal(err)
		}
		if err = permit.Consume(); err != nil {
			t.Fatal(err)
		}
		if released := piConsumerLeaseReleased(t, registry, stateRoot, td.TaskID); released {
			t.Fatal("lease probe cannot observe a held runner lease")
		}
		t.SkipNow()
	})
	if released := piConsumerLeaseReleased(t, registry, stateRoot, strings.Repeat("f", 32)); !released {
		t.Fatal("failed sealing retained the task handle or runner lease")
	}
	store, err := taskdir.OpenStoreWithPredicates(stateRoot, registry)
	if err != nil {
		t.Fatalf("store handle was retained after failed sealing: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
}

// piConsumerLeaseReleased reports the runner lease state through fresh
// handles, which close before returning.
func piConsumerLeaseReleased(t *testing.T, registry predicate.Registry, stateRoot, taskID string) bool {
	t.Helper()
	store, err := taskdir.OpenStoreWithPredicates(stateRoot, registry)
	if err != nil {
		t.Fatal(err)
	}
	closeStore := piConsumerCloser(t, "probe store", store)
	defer closeStore()
	probe, err := store.OpenTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	closeProbe := piConsumerCloser(t, "probe task", probe)
	defer closeProbe()
	released, err := probe.RunnerLeaseReleased()
	if err != nil {
		t.Fatal(err)
	}
	return released
}

// piConsumerTaskFiles reads the sealed evidence and records a recheck must
// never change.
func piConsumerTaskFiles(t *testing.T, root, taskID string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range []string{"task.json", "meta.json", "provider.ref.json", "raw/stdout", "raw/stderr", "provider.exit", "outcome.json"} {
		data, err := os.ReadFile(filepath.Join(root, "tasks", taskID, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	return files
}

func TestPiConsumerOfflineRecheck(t *testing.T) {
	registry := NativeCatalog().Registry()
	settled := []string{`{"type":"agent_settled","aborted":false}`}
	abortedSettlement := []string{`{"type":"agent_settled","aborted":true}`}
	display := []string{`{"type":"session_info_changed","name":"renamed after settlement"}`}
	session := []string{piConsumerSessionLine(piConsumerSession)}
	cases := []struct {
		name       string
		stream     string
		wantCode   int
		wantAnswer string
	}{
		{name: "complete cycle", stream: piConsumerStream(session, piConsumerCycle(piConsumerAnswer), settled, display), wantAnswer: piConsumerAnswer},
		{name: "explicit abort", stream: piConsumerStream(session, piConsumerCycle(piConsumerAnswer), abortedSettlement, display), wantCode: 4},
		{name: "explicit retry failure in the active cycle", stream: piConsumerStream(session, piConsumerRetryCycles(piConsumerAnswer, `{"type":"auto_retry_end","attempt":1,"success":false,"finalError":"gave up"}`), settled, display), wantCode: 4},
	}
	for _, mode := range piConsumerModes {
		version := map[string]string{pi.ModeReadOnly: "6", pi.ModeWorkspaceWrite: "8"}[mode]
		source := piConsumerRecheckSource(t, mode, version)
		target := pi.ReferenceForMode(mode)
		recovery, err := registry.ResolveRecovery(source)
		if err != nil || !recovery.Target().Equal(target) || target.Version != "9" {
			t.Fatalf("%s v%s recovery target=%+v err=%v want v9 %+v", mode, version, recovery, err, target)
		}
		for _, testCase := range cases {
			t.Run(mode+"/"+testCase.name, func(t *testing.T) {
				assertPiConsumerRecheck(t, registry, source, target, testCase.stream, testCase.wantCode, testCase.wantAnswer)
			})
		}
	}
}

func assertPiConsumerRecheck(t *testing.T, registry predicate.Registry, source, target task.PredicateRef, stream string, wantCode int, wantAnswer string) {
	t.Helper()
	launched := filepath.Join(t.TempDir(), "provider-launched")
	cliDir := t.TempDir()
	writeRunnerFixture(t, filepath.Join(cliDir, "pi"), "#!/bin/sh\n: > '"+launched+"'\n")
	t.Setenv("PATH", cliDir)
	root, taskID, original := sealedPiRecheckTask(t, registry, source, stream)
	if original.Verdict != task.VerdictRejected {
		t.Fatalf("source %s was not rejected by its own policy: %+v", source.Version, original)
	}
	before := piConsumerTaskFiles(t, root, taskID)

	first := runRecheckTestCommand(t, registry, root, taskID, "recheck", wantCode)
	assertPiConsumerRecheckResult(t, first, target, wantCode, wantAnswer)
	reportBefore := readPiConsumerReport(t, first)
	second := runRecheckTestCommand(t, registry, root, taskID, "recheck", wantCode)
	if !reflect.DeepEqual(first.Recheck, second.Recheck) || !reflect.DeepEqual(reportBefore, readPiConsumerReport(t, second)) {
		t.Fatalf("repeated recheck changed its assessment: first=%+v second=%+v", first.Recheck, second.Recheck)
	}
	if first.Recheck.SourcePredicate != source || first.Recheck.SourceEvidenceSHA256 != original.EvidenceSHA256 ||
		first.Recheck.SourceOutcomeSHA256 != task.ComputeSHA256(before["outcome.json"]) || first.Recheck.SourceSealSHA256 != task.ComputeSHA256(before["provider.exit"]) {
		t.Fatalf("report does not bind the exact source: %+v", first.Recheck)
	}
	if collect := runRecheckTestCommand(t, registry, root, taskID, "collect", 4); collect.Publication != task.PublicationRejected.String() {
		t.Fatalf("original rejection changed: %+v", collect)
	}
	after := piConsumerTaskFiles(t, root, taskID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("recheck changed sealed task evidence or the original outcome")
	}
	if _, err := os.Stat(launched); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline recheck launched a provider: %v", err)
	}
}

func assertPiConsumerRecheckResult(t *testing.T, response Response, target task.PredicateRef, wantCode int, wantAnswer string) {
	t.Helper()
	if wantCode == 0 {
		assertRecoveredResponse(t, response, target, wantAnswer)
		return
	}
	recheck := response.Recheck
	if recheck == nil || recheck.Status != task.RecheckStatusRejected || recheck.Target != target || recheck.Answer != nil || recheck.AnswerPath != "" {
		t.Fatalf("recheck did not reject without an answer: %+v", recheck)
	}
	if len(recheck.Diagnostics) != 1 || recheck.Diagnostics[0].Code != "provider_failed" {
		t.Fatalf("recheck diagnostics=%+v want provider_failed", recheck.Diagnostics)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(recheck.ReportPath), "result.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected recheck published an answer: %v", err)
	}
}

func readPiConsumerReport(t *testing.T, response Response) []byte {
	t.Helper()
	data, err := os.ReadFile(response.Recheck.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
