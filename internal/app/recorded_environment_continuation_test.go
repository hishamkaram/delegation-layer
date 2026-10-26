package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/antigravity"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/provider/pi"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// launchSelectors are the caller-visible variables that choose a provider's
// configuration, credentials, sessions, and writable roots. The predecessor is
// admitted under one set; the continuing command runs under another.
type launchSelectors struct {
	home, codexHome, configHome, dataHome, tmpDir string
}

func newLaunchSelectors(t *testing.T, root, name string) launchSelectors {
	t.Helper()
	selectors := launchSelectors{
		home: filepath.Join(root, name+"-home"), codexHome: filepath.Join(root, name+"-codex"),
		configHome: filepath.Join(root, name+"-config"), dataHome: filepath.Join(root, name+"-data"),
		tmpDir: filepath.Join(root, name+"-tmp"),
	}
	for _, path := range selectors.paths() {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return selectors
}

// scopePath is every scope's PATH: the fixture CLI directory first, then the
// system directories the fake supervisor's utilities need.
func scopePath(cliDir string) string {
	return cliDir + ":/usr/bin:/bin"
}

func (s launchSelectors) paths() []string {
	return []string{s.home, s.codexHome, s.configHome, s.dataHome, s.tmpDir}
}

// apply replaces the whole process environment with this scope. Each scope
// starts empty, so the recorded and caller environments hold only the values
// the test chose and no ambient credential can reach a record.
func (s launchSelectors) apply(t *testing.T, cliDir string) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", s.home)
	t.Setenv("CODEX_HOME", s.codexHome)
	t.Setenv("XDG_CONFIG_HOME", s.configHome)
	t.Setenv("XDG_DATA_HOME", s.dataHome)
	t.Setenv("TMPDIR", s.tmpDir)
	t.Setenv("PATH", scopePath(cliDir))
	t.Setenv("DELEGATE_PUEUE_CONFIG", filepath.Join(s.home, "scope-pueue.yml"))
}

// recordedEnvironmentCase names one production provider registration and the
// sealed stream that gives its predecessor an exact resumable session. A
// nonempty predicateVersion is the output contract both tasks must carry, and
// model is the recorded alias a provider that requires one is admitted with.
type recordedEnvironmentCase struct {
	name, provider, binary, mode string
	predicateVersion, model      string
	// session is the exact session the predecessor's sealed stream reports.
	session func(rootID, taskID string) (string, error)
	// stream is that sealed stream, which publishes an answer.
	stream func(session, workspace string) []string
}

// recordedEnvironmentCases covers every launchable continuation registration
// of the production catalog in both permission modes.
func recordedEnvironmentCases() []recordedEnvironmentCase {
	var cases []recordedEnvironmentCase
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		claudeFamily := outputUpgradeCase{mode: mode}
		claudeStream := func(session, _ string) []string {
			return []string{claudeFamily.initLine(session), claudeFamily.resultLine(session, outputUpgradeAnswer, "")}
		}
		cases = append(cases, recordedEnvironmentCase{
			name: "codex/" + mode, provider: "codex:exec", binary: "codex", mode: mode,
			session: fixedRecordedSession(recordedEnvironmentCodexThread), stream: recordedCodexStream,
		}, recordedEnvironmentCase{
			// A fresh OpenCode predecessor is an ACP task, so its continuation is
			// ACP to ACP under the same capability and output contract.
			name: "opencode/" + mode, provider: openCodeProvider, binary: "opencode", mode: mode, predicateVersion: openCodeACPVersion,
			session: fixedRecordedSession(outputUpgradeOpenCodeSession),
			stream: func(session, _ string) []string {
				return strings.Split(strings.TrimSuffix(openCodeACPStream(mode, session, true), "\n"), "\n")
			},
		}, recordedEnvironmentCase{
			name: "claude/" + mode, provider: claude.Provider, binary: "claude", mode: mode,
			session: claude.FreshSessionID, stream: claudeStream,
		}, recordedEnvironmentCase{
			// CCR admission normally runs a supervised launch-help inspection;
			// this hermetic harness uses the existing static CCR substitution.
			name: "ccr/" + mode, provider: ccr.Provider, binary: "ccr", mode: mode, model: "fixture-alias",
			session: claude.FreshSessionID, stream: claudeStream,
		}, recordedEnvironmentCase{
			name: "pi/" + mode, provider: pi.Provider, binary: "pi", mode: mode,
			session: fixedRecordedSession(recordedEnvironmentPiSession),
			stream: func(session, workspace string) []string {
				return strings.Split(string(interactivePiSuccessStream(session, workspace)), "\n")
			},
		}, recordedEnvironmentCase{
			name: "antigravity/" + mode, provider: antigravity.Provider, binary: "agy", mode: mode,
			session: fixedRecordedSession(outputUpgradeAGYConversation),
			stream: func(session, _ string) []string {
				return []string{fmt.Sprintf(`{"response":%q,"conversation_id":%q,"status":"SUCCESS"}`, outputUpgradeAnswer, session)}
			},
		})
	}
	return cases
}

const (
	recordedEnvironmentCodexThread = "123e4567-e89b-12d3-a456-426614174000"
	recordedEnvironmentPiSession   = "11111111-1111-4111-8111-111111111111"
)

func fixedRecordedSession(session string) func(string, string) (string, error) {
	return func(string, string) (string, error) { return session, nil }
}

func recordedCodexStream(session, _ string) []string {
	return []string{
		fmt.Sprintf(`{"type":"thread.started","thread_id":%q}`, session),
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"final","type":"agent_message"}}`,
		`{"type":"item.completed","item":{"id":"final","type":"agent_message","text":"answer"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}
}

// catalog is the production catalog, with CCR's static admission
// substitution extended to the runner's preflight.
func (c recordedEnvironmentCase) catalog(t *testing.T) commonprovider.Catalog {
	t.Helper()
	if c.provider == ccr.Provider {
		return recordedCCRCatalog(t)
	}
	return NativeCatalog()
}

// Hook kinds name the production preparation hook that produced a capture.
const (
	hookExisting  = "existing"
	hookSuccessor = "successor"
)

// candidateCapture is one candidate returned by a wrapped production hook,
// with the hook kind and the task the hook was asked to prepare.
type candidateCapture struct {
	hook      string
	taskID    string
	candidate commonprovider.ProfileCandidate
}

type recordedEnvironmentFixture struct {
	testCase   recordedEnvironmentCase
	root       string
	workspace  string
	runner     string
	cliDir     string
	supervisor *pueue.Client
	// commandLog and statusPath belong to the external supervisor fixture;
	// submissions records the private pair's stashed adds.
	commandLog, statusPath, submissions string
	saved, caller                       launchSelectors
	// ambient is a third scope that reconstructs both admitted tasks later.
	ambient  launchSelectors
	captures []candidateCapture
	// successorHook records whether the production registration declares its
	// own PrepareSuccessor; a fallback-only registration is never given one.
	successorHook bool
	deps          Dependencies
	predecessorID string
	// session is the exact session the sealed predecessor reports.
	session     string
	predecessor map[string][]byte
}

// recordedSupervisorKind selects the supervisor binding the predecessor is
// admitted under: an explicit external configuration or the state root's
// private pair.
type recordedSupervisorKind uint8

const (
	recordedExternalSupervisor recordedSupervisorKind = iota
	recordedPrivateSupervisor
)

// recordedFixtureOptions are the few choices a recorded-environment fixture
// may vary; the zero value is an external supervisor and a provider CLI that
// only exits 0.
type recordedFixtureOptions struct {
	supervisor recordedSupervisorKind
	// providerScript, when set, is the caller-visible provider CLI.
	providerScript string
	// installedRunner selects, for the successor only, this test binary
	// installed as the state root's protected managed runner.
	installedRunner bool
}

func newRecordedEnvironmentFixture(t *testing.T, testCase recordedEnvironmentCase) *recordedEnvironmentFixture {
	t.Helper()
	return newRecordedEnvironmentFixtureWith(t, testCase, recordedFixtureOptions{})
}

func newRecordedEnvironmentFixtureWith(t *testing.T, testCase recordedEnvironmentCase, options recordedFixtureOptions) *recordedEnvironmentFixture {
	t.Helper()
	// The provider writable roots include /tmp, and static admission rejects a
	// state root inside them, so the whole fixture lives under the user's home.
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(mustCCRContinuationTempDir(t, userHome, ".delegate-recorded-environment-"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &recordedEnvironmentFixture{
		testCase: testCase, root: filepath.Join(base, "state"), workspace: filepath.Join(base, "workspace"),
		runner: filepath.Join(base, "delegate-run"), cliDir: filepath.Join(base, "cli"),
		saved: newLaunchSelectors(t, base, "saved"), caller: newLaunchSelectors(t, base, "caller"), ambient: newLaunchSelectors(t, base, "ambient"),
		predecessorID: strings.Repeat("a", 32),
	}
	for _, dir := range []string{fixture.workspace, fixture.cliDir} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	providerScript := options.providerScript
	if providerScript == "" {
		providerScript = "#!/bin/sh\nexit 0\n"
	}
	writeRunnerFixture(t, filepath.Join(fixture.cliDir, testCase.binary), providerScript)
	supervisorOptions, clientPath := fixture.bindSupervisor(t, options.supervisor)
	t.Cleanup(func() {
		if err := fixture.supervisor.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.deps = Dependencies{
		Catalog: fixture.capturingCatalog(t), SupervisorOptions: supervisorOptions, RunnerExecutable: fixture.runner,
		InitialSupervisorExecutable: clientPath,
	}
	fixture.saved.apply(t, fixture.cliDir)
	fixture.admitSealedPredecessor(t)
	if options.installedRunner {
		fixture.installRunner(t)
	}
	fixture.caller.apply(t, fixture.cliDir)
	return fixture
}

// bindSupervisor binds the fixture's supervisor of kind and returns its
// command options and client executable. The private pair needs the state
// root's identity, so that kind creates the store first.
func (f *recordedEnvironmentFixture) bindSupervisor(t *testing.T, kind recordedSupervisorKind) (pueue.Options, string) {
	t.Helper()
	if kind == recordedExternalSupervisor {
		var options pueue.Options
		f.supervisor, options, f.statusPath, f.commandLog = newOrdinarySubmissionReceiptSupervisor(t)
		return options, f.supervisor.Binding().ClientExecutable
	}
	store, err := taskdir.InitStoreWithPredicates(f.root, f.testCase.catalog(t).Registry())
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.RootID
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	supervisor, options, clientPath := bindRefusedContinuePair(t, f.root, rootID)
	f.supervisor, f.submissions = supervisor, filepath.Join(filepath.Dir(supervisor.Binding().ConfigPath), "submissions")
	return options, clientPath
}

// capturingCatalog wraps the production registration so the test sees every
// candidate the continuation prepares, without replacing its behavior. It
// observes the registration's own PrepareExisting and, only when the
// registration already declares one, its own PrepareSuccessor.
func (f *recordedEnvironmentFixture) capturingCatalog(t *testing.T) commonprovider.Catalog {
	t.Helper()
	registration, err := f.testCase.catalog(t).Lookup(f.testCase.provider)
	if err != nil {
		t.Fatal(err)
	}
	registration.PrepareExisting = f.capture(hookExisting, registration.PrepareExisting)
	if registration.PrepareSuccessor != nil {
		f.successorHook = true
		registration.PrepareSuccessor = f.capture(hookSuccessor, registration.PrepareSuccessor)
	}
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// capture returns prepare unchanged in behavior and records what it returns.
func (f *recordedEnvironmentFixture) capture(hook string, prepare commonprovider.PrepareExistingCandidate) commonprovider.PrepareExistingCandidate {
	return func(request task.TaskRecord, meta task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
		candidate, err := prepare(request, meta, environment)
		if err == nil {
			f.captures = append(f.captures, candidateCapture{hook: hook, taskID: request.TaskID, candidate: candidate})
		}
		return candidate, err
	}
}

func (f *recordedEnvironmentFixture) admitSealedPredecessor(t *testing.T) {
	t.Helper()
	store, err := openStore(f.root, f.deps.storeDependencies(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	brief := []byte("continue the recorded provider session")
	request, err := admission.BuildRequest(store.RootID, f.predecessorID, f.testCase.provider, f.workspace, task.TaskConfig{
		Permission: f.testCase.mode, Budget: "1m0s", Model: f.testCase.model,
	}, brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.deps.Catalog.Candidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	profile, err := admission.ProfileForAdmission(candidate, request, true)
	if err != nil {
		t.Fatal(err)
	}
	meta := tasklifecycle.NewMeta(request, profile, f.supervisor.Binding(), f.runner, task.RunnerOwnershipManaged, "recorded-environment-test")
	if f.session, err = f.testCase.session(store.RootID, f.predecessorID); err != nil {
		t.Fatal(err)
	}
	td, err := store.CreateTask(f.predecessorID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	f.sealPredecessor(t, td, meta.Predicate)
	f.predecessor = readImmutableRecords(t, td.Dir)
}

func (f *recordedEnvironmentFixture) sealPredecessor(t *testing.T, td *taskdir.TaskDir, reference task.PredicateRef) {
	t.Helper()
	start, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	arguments := slices.Repeat([]string{"argument"}, 32)
	for _, output := range meta.OutputArtifacts {
		arguments[output.ArgumentIndex] = ""
	}
	if _, err = td.PrepareLaunchFiles(arguments); err != nil {
		t.Fatalf("prepare launch files: %v", err)
	}
	if err = td.RecordProviderIdentity(task.SessionIdentity{Provider: f.testCase.provider, ConversationID: f.session}); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles(strings.Join(f.testCase.stream(f.session, f.workspace), "\n"), ""); err != nil {
		t.Fatal(err)
	}
	if err = td.ImportOutputArtifacts(); err != nil {
		t.Fatalf("import output artifacts: %v", err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", reference); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err = start.Release(); err != nil {
		t.Fatal(err)
	}
	requireCommittedPredecessor(t, td, reference)
}

// requireCommittedPredecessor collects the sealed predecessor; its stream must
// publish the answer so the continuation starts from a successful session.
func requireCommittedPredecessor(t *testing.T, td *taskdir.TaskDir, reference task.PredicateRef) {
	t.Helper()
	outcome, cleanupErr, collectErr := td.Collect(reference)
	if cleanupErr != nil || collectErr != nil || outcome == nil || outcome.Verdict != task.VerdictCommitted {
		t.Fatalf("sealing predecessor cleanup=%v collect=%v outcome=%+v", cleanupErr, collectErr, outcome)
	}
}

func readImmutableRecords(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	records := make(map[string][]byte)
	for _, name := range []string{"task.json", "meta.json", "brief.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		records[name] = data
	}
	return records
}

// entryPointArguments is the public argv of entryPoint. A nonempty
// configPath is passed as an explicit --pueue-config.
func (f *recordedEnvironmentFixture) entryPointArguments(t *testing.T, entryPoint, configPath string) []string {
	t.Helper()
	args := []string{"--root", f.root}
	if configPath != "" {
		args = append(args, "--pueue-config", configPath)
	}
	switch entryPoint {
	case "continue --task":
		return append(args, "continue", "--task", f.predecessorID, "--json")
	case "dispatch --resume-task":
		brief := filepath.Join(f.workspace, "follow-up.md")
		if err := os.WriteFile(brief, []byte("continue the recorded provider session"), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "dispatch", "--provider", f.testCase.provider, "--brief", brief, "--cwd", f.workspace,
			"--resume-task", f.predecessorID, "--permission", f.testCase.mode, "--budget", "1m0s", "--json")
		if f.testCase.model != "" {
			args = append(args, "--model", f.testCase.model)
		}
		return args
	default:
		t.Fatalf("unknown entry point %q", entryPoint)
		return nil
	}
}

// TestContinuationAdmitsSuccessorInCallerLaunchEnvironment proves that both
// public commands admit the new successor in the caller's captured scope: the
// predecessor's record decides compatibility only. Static validation and
// creation compile the same candidate, an explicit --pueue-config keeps
// precedence over the caller scope's DELEGATE_PUEUE_CONFIG, and each admitted
// record later reconstructs from its own saved scope under a third ambient one.
func TestContinuationAdmitsSuccessorInCallerLaunchEnvironment(t *testing.T) {
	for _, testCase := range recordedEnvironmentCases() {
		for _, entryPoint := range []string{"continue --task", "dispatch --resume-task"} {
			t.Run(testCase.name+"/"+entryPoint, func(t *testing.T) {
				fixture := newRecordedEnvironmentFixture(t, testCase)
				code, response := fixture.run(t, entryPoint, fixture.supervisor.Binding().ConfigPath)
				if code != 0 || response.TaskRecord != "created" || response.TaskID == "" {
					t.Fatalf("%s exit=%d response=%+v", entryPoint, code, response)
				}
				// The command's own admission is what the hook assertion judges; the
				// worker rebuild inside assertSuccessor adds later captures.
				admitted := slices.Clone(fixture.captures)
				successor := readImmutableRecords(t, fixture.taskDir(response.TaskID))
				fixture.assertSuccessor(t, response.TaskID)
				fixture.assertAdmissionHook(t, admitted, response.TaskID)
				fixture.assertReconstructedCandidates(t, response.TaskID)
				fixture.assertCallerEnvironmentRestored(t)
				fixture.assertAmbientReconstruction(t, response.TaskID)
				fixture.assertPredecessorUnchanged(t)
				if got := readImmutableRecords(t, fixture.taskDir(response.TaskID)); !maps.EqualFunc(got, successor, bytes.Equal) {
					t.Fatal("successor records changed after admission")
				}
				assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
			})
		}
	}
}

// run executes entryPoint through the public command and decodes its JSON
// response; configPath is as for entryPointArguments.
func (f *recordedEnvironmentFixture) run(t *testing.T, entryPoint, configPath string) (int, Response) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(f.entryPointArguments(t, entryPoint, configPath), &stdout, &stderr, f.deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("%s exit=%d stdout=%q stderr=%q: %v", entryPoint, code, stdout.String(), stderr.String(), err)
	}
	return code, response
}

func (f *recordedEnvironmentFixture) taskDir(id string) string {
	return filepath.Join(f.root, "tasks", id)
}

func (f *recordedEnvironmentFixture) loadRecords(t *testing.T, id string) (request *task.TaskRecord, meta *task.MetaRecord) {
	t.Helper()
	store, err := openStore(f.root, f.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	td, err := store.OpenTask(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	request, meta, err = td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	return request, meta
}

func (f *recordedEnvironmentFixture) assertSuccessor(t *testing.T, id string) {
	t.Helper()
	predecessorRequest, predecessorMeta := f.loadRecords(t, f.predecessorID)
	request, meta := f.loadRecords(t, id)
	if request.PriorSession == nil || request.PriorSession.ConversationID != f.session || request.PriorSession.PredecessorTaskID != f.predecessorID {
		t.Fatalf("successor lost the exact session identity: %+v", request.PriorSession)
	}
	if !meta.EnvironmentRecorded || slices.Equal(meta.Environment, predecessorMeta.Environment) {
		t.Fatalf("successor environment %s recorded=%t copied the predecessor's %s", summarizeValues(meta.Environment), meta.EnvironmentRecorded, summarizeValues(predecessorMeta.Environment))
	}
	requireNoSelector(t, "successor metadata environment", meta.Environment, f.saved.paths())
	requireSelector(t, "successor metadata environment", meta.Environment, f.caller.home)
	if meta.EffectiveConfig.Policy == nil || predecessorMeta.EffectiveConfig.Policy == nil {
		t.Fatalf("admitted policy missing (successor=%t predecessor=%t)", meta.EffectiveConfig.Policy != nil, predecessorMeta.EffectiveConfig.Policy != nil)
	}
	requireNoSelector(t, "successor writable roots", meta.EffectiveConfig.Policy.WritableRoots, f.saved.paths())
	if request.Mode != predecessorRequest.Mode {
		t.Fatalf("successor mode=%q differs from predecessor mode=%q", request.Mode, predecessorRequest.Mode)
	}
	if version := f.testCase.predicateVersion; version != "" && (predecessorMeta.Predicate.Version != version || !meta.Predicate.Equal(predecessorMeta.Predicate)) {
		t.Fatalf("successor predicate %+v and predecessor predicate %+v, want both %s", meta.Predicate, predecessorMeta.Predicate, version)
	}
	f.assertWorkerRebuild(t, request, meta)
}

// assertWorkerRebuild repeats the managed worker's own reconstruction: it
// selects the saved launch scope and must match the persisted successor
// without changing the process environment.
func (f *recordedEnvironmentFixture) assertWorkerRebuild(t *testing.T, request *task.TaskRecord, meta *task.MetaRecord) {
	t.Helper()
	ambient := os.Environ()
	scoped, err := scopedDependencies(f.deps.withCapturedEnvironment(), meta.Environment, meta.EnvironmentRecorded)
	if err != nil {
		t.Fatal(err)
	}
	_, matchErr := prepareMatchedProfile(scoped, f.root, *request, *meta)
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("worker reconstruction changed the process environment")
	}
	if matchErr != nil {
		t.Fatalf("worker reconstruction no longer matches the persisted successor: %v", matchErr)
	}
}

// assertAdmissionHook requires the command's own admission of the new
// successor to have reached the real hook that prepares it: the registration's
// PrepareSuccessor when it declares one, otherwise the PrepareExisting hook its
// fallback reconstructs through. A fallback-only registration never sees a
// successor-kind capture.
func (f *recordedEnvironmentFixture) assertAdmissionHook(t *testing.T, admitted []candidateCapture, successorID string) {
	t.Helper()
	want := hookExisting
	if f.successorHook {
		want = hookSuccessor
	}
	reached, wrongKind := 0, 0
	for _, capture := range admitted {
		switch {
		case capture.taskID == successorID && capture.hook == want:
			reached++
		case capture.hook == hookSuccessor && !f.successorHook:
			wrongKind++
		}
	}
	if reached == 0 || wrongKind != 0 {
		t.Fatalf("admission of the new successor reached its %s hook %d times (successor hook declared: %t, %d captures, %d unexpected successor-kind)",
			want, reached, f.successorHook, len(admitted), wrongKind)
	}
}

// assertReconstructedCandidates requires every prepared candidate to carry
// the launch scope recorded by the task it was prepared for: the new
// successor's static validation, creation, and rebuild share its own caller
// scope, and any predecessor reconstruction keeps the predecessor's.
func (f *recordedEnvironmentFixture) assertReconstructedCandidates(t *testing.T, successorID string) {
	t.Helper()
	records := map[string]*task.MetaRecord{}
	_, records[f.predecessorID] = f.loadRecords(t, f.predecessorID)
	_, records[successorID] = f.loadRecords(t, successorID)
	if len(f.captures) == 0 {
		t.Fatal("the continuation never prepared the provider candidate")
	}
	executables := map[string]string{}
	for index, capture := range f.captures {
		meta, ok := records[capture.taskID]
		if !ok {
			t.Fatalf("%s candidate %d prepared an unexpected task", capture.hook, index)
		}
		inspection := capture.candidate.Inspection
		if !slices.Equal(inspection.Environment, meta.Environment) {
			t.Fatalf("%s candidate %d inspection environment %s want its task's recorded %s", capture.hook, index, summarizeValues(inspection.Environment), summarizeValues(meta.Environment))
		}
		if !slices.Equal(capture.candidate.WritableRoots, meta.EffectiveConfig.Policy.WritableRoots) {
			t.Fatalf("%s candidate %d writable roots %s want its task's recorded %s", capture.hook, index, summarizeValues(capture.candidate.WritableRoots), summarizeValues(meta.EffectiveConfig.Policy.WritableRoots))
		}
		if previous, seen := executables[capture.taskID]; seen && previous != inspection.Executable {
			t.Fatalf("%s candidate %d executable differs from an earlier candidate of the same task", capture.hook, index)
		}
		executables[capture.taskID] = inspection.Executable
	}
}

func (f *recordedEnvironmentFixture) assertCallerEnvironmentRestored(t *testing.T) {
	t.Helper()
	for key, want := range map[string]string{
		"HOME": f.caller.home, "CODEX_HOME": f.caller.codexHome, "XDG_CONFIG_HOME": f.caller.configHome,
		"XDG_DATA_HOME": f.caller.dataHome, "TMPDIR": f.caller.tmpDir, "PATH": scopePath(f.cliDir),
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("caller %s was not restored after the command", key)
		}
	}
}

// assertAmbientReconstruction moves the process to a third scope and rebuilds
// both admitted tasks: each record owns its launch scope, so neither adopts
// the ambient values.
func (f *recordedEnvironmentFixture) assertAmbientReconstruction(t *testing.T, successorID string) {
	t.Helper()
	f.ambient.apply(t, f.cliDir)
	for _, id := range []string{f.predecessorID, successorID} {
		request, meta := f.loadRecords(t, id)
		requireNoSelector(t, "admitted environment", meta.Environment, f.ambient.paths())
		f.assertWorkerRebuild(t, request, meta)
	}
}

func (f *recordedEnvironmentFixture) assertPredecessorUnchanged(t *testing.T) {
	t.Helper()
	store, err := openStore(f.root, f.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	td, err := store.OpenTask(f.predecessorID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	for name, want := range f.predecessor {
		got, readErr := os.ReadFile(filepath.Join(td.Dir, name))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("predecessor %s changed: err=%v", name, readErr)
		}
	}
}

// summarizeValues reports only a count so a failure never prints environment
// entries or host paths; the equality assertion itself carries the comparison.
func summarizeValues(values []string) string {
	return fmt.Sprintf("%d entries", len(values))
}

// requireNoSelector reports the leaking selector's position in paths and the
// entry index, never the entry itself.
func requireNoSelector(t *testing.T, label string, values, paths []string) {
	t.Helper()
	for valueIndex, value := range values {
		for pathIndex, path := range paths {
			if strings.Contains(value, path) {
				t.Fatalf("%s entry %d leaked caller selector #%d", label, valueIndex, pathIndex)
			}
		}
	}
}

// requireSelector reports only whether some entry carries the selector.
func requireSelector(t *testing.T, label string, values []string, path string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(value, path) {
			return
		}
	}
	t.Fatalf("%s does not carry the caller selector (%s)", label, summarizeValues(values))
}
