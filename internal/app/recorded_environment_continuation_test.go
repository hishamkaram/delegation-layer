package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
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

func (s launchSelectors) paths() []string {
	return []string{s.home, s.codexHome, s.configHome, s.dataHome, s.tmpDir}
}

// inheritedLaunchKeys are the variables any registered provider profile can
// retain. Each fixture clears them so the recorded and caller environments
// hold only the values the test chose.
var inheritedLaunchKeys = []string{
	"HOME", "CODEX_HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING",
	"XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH",
	"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_TUI_CONFIG", "OPENCODE_PERMISSION", "OPENCODE_AUTO_SHARE",
}

func (s launchSelectors) apply(t *testing.T, cliDir string) {
	t.Helper()
	for _, key := range inheritedLaunchKeys {
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
	t.Setenv("PATH", cliDir)
}

// recordedEnvironmentCase names one production provider registration and the
// sealed stream that gives its predecessor an exact resumable session. A
// nonempty predicateVersion is the output contract both tasks must carry.
type recordedEnvironmentCase struct {
	name, provider, binary, mode, session string
	predicateVersion                      string
	stream                                []string
}

func recordedEnvironmentCases() []recordedEnvironmentCase {
	var cases []recordedEnvironmentCase
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		const codexThread = "123e4567-e89b-12d3-a456-426614174000"
		cases = append(cases, recordedEnvironmentCase{
			name: "codex/" + mode, provider: "codex:exec", binary: "codex", mode: mode, session: codexThread,
			stream: []string{
				fmt.Sprintf(`{"type":"thread.started","thread_id":%q}`, codexThread),
				`{"type":"turn.started"}`,
				`{"type":"item.started","item":{"id":"final","type":"agent_message"}}`,
				`{"type":"item.completed","item":{"id":"final","type":"agent_message","text":"answer"}}`,
				`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
			},
		}, recordedEnvironmentCase{
			// A fresh OpenCode predecessor is an ACP task, so its continuation is
			// ACP to ACP under the same capability and output contract.
			name: "opencode/" + mode, provider: openCodeProvider, binary: "opencode", mode: mode, session: outputUpgradeOpenCodeSession,
			predicateVersion: openCodeACPVersion,
			stream:           strings.Split(strings.TrimSuffix(openCodeACPStream(mode, outputUpgradeOpenCodeSession, true), "\n"), "\n"),
		})
	}
	return cases
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
	testCase      recordedEnvironmentCase
	root          string
	workspace     string
	runner        string
	cliDir        string
	supervisor    *pueue.Client
	commandLog    string
	saved, caller launchSelectors
	captures      []candidateCapture
	// successorHook records whether the production registration declares its
	// own PrepareSuccessor; a fallback-only registration is never given one.
	successorHook bool
	deps          Dependencies
	predecessorID string
	predecessor   map[string][]byte
}

func newRecordedEnvironmentFixture(t *testing.T, testCase recordedEnvironmentCase) *recordedEnvironmentFixture {
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
		saved: newLaunchSelectors(t, base, "saved"), caller: newLaunchSelectors(t, base, "caller"), predecessorID: strings.Repeat("a", 32),
	}
	for _, dir := range []string{fixture.workspace, fixture.cliDir} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	writeRunnerFixture(t, filepath.Join(fixture.cliDir, testCase.binary), "#!/bin/sh\nexit 0\n")
	var options pueue.Options
	fixture.supervisor, options, _, fixture.commandLog = newOrdinarySubmissionReceiptSupervisor(t)
	t.Cleanup(func() {
		if err := fixture.supervisor.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.deps = Dependencies{
		Catalog: fixture.capturingCatalog(t), SupervisorOptions: options, RunnerExecutable: fixture.runner,
		InitialSupervisorExecutable: fixture.supervisor.Binding().ClientExecutable,
	}
	fixture.saved.apply(t, fixture.cliDir)
	fixture.admitSealedPredecessor(t)
	fixture.caller.apply(t, fixture.cliDir)
	return fixture
}

// capturingCatalog wraps the production registration so the test sees every
// candidate the continuation prepares, without replacing its behavior. It
// observes the registration's own PrepareExisting and, only when the
// registration already declares one, its own PrepareSuccessor.
func (f *recordedEnvironmentFixture) capturingCatalog(t *testing.T) commonprovider.Catalog {
	t.Helper()
	registration, err := NativeCatalog().Lookup(f.testCase.provider)
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
		Permission: f.testCase.mode, Budget: "1m0s",
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
	if err = td.RecordProviderIdentity(task.SessionIdentity{Provider: f.testCase.provider, ConversationID: f.testCase.session}); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles(strings.Join(f.testCase.stream, "\n"), ""); err != nil {
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

func (f *recordedEnvironmentFixture) entryPointArguments(t *testing.T, entryPoint string) []string {
	t.Helper()
	args := []string{"--root", f.root, "--pueue-config", f.supervisor.Binding().ConfigPath}
	switch entryPoint {
	case "continue --task":
		return append(args, "continue", "--task", f.predecessorID, "--json")
	case "dispatch --resume-task":
		brief := filepath.Join(f.workspace, "follow-up.md")
		if err := os.WriteFile(brief, []byte("continue the recorded provider session"), 0o600); err != nil {
			t.Fatal(err)
		}
		return append(args, "dispatch", "--provider", f.testCase.provider, "--brief", brief, "--cwd", f.workspace,
			"--resume-task", f.predecessorID, "--permission", f.testCase.mode, "--budget", "1m0s", "--json")
	default:
		t.Fatalf("unknown entry point %q", entryPoint)
		return nil
	}
}

func TestContinuationUsesRecordedLaunchEnvironmentNotCallerEnvironment(t *testing.T) {
	for _, testCase := range recordedEnvironmentCases() {
		for _, entryPoint := range []string{"continue --task", "dispatch --resume-task"} {
			t.Run(testCase.name+"/"+entryPoint, func(t *testing.T) {
				fixture := newRecordedEnvironmentFixture(t, testCase)
				var stdout, stderr bytes.Buffer
				code := Run(fixture.entryPointArguments(t, entryPoint), &stdout, &stderr, fixture.deps)
				var response Response
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatalf("%s exit=%d stdout=%q stderr=%q: %v", entryPoint, code, stdout.String(), stderr.String(), err)
				}
				if code != 0 || response.TaskRecord != "created" || response.TaskID == "" {
					t.Fatalf("%s exit=%d response=%+v stdout=%s stderr=%q", entryPoint, code, response, stdout.String(), stderr.String())
				}
				// The command's own admission is what the hook assertion judges; the
				// worker rebuild inside assertSuccessor adds later captures.
				admitted := slices.Clone(fixture.captures)
				fixture.assertSuccessor(t, response.TaskID)
				fixture.assertAdmissionHook(t, admitted, response.TaskID)
				fixture.assertReconstructedCandidates(t)
				fixture.assertCallerEnvironmentRestored(t)
				fixture.assertPredecessorUnchanged(t)
				assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
			})
		}
	}
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
	if request.PriorSession == nil || request.PriorSession.ConversationID != f.testCase.session || request.PriorSession.PredecessorTaskID != f.predecessorID {
		t.Fatalf("successor lost the exact session identity: %+v", request.PriorSession)
	}
	if !slices.Equal(meta.Environment, predecessorMeta.Environment) || !meta.EnvironmentRecorded {
		t.Fatalf("successor environment %s recorded=%t want predecessor's %s", summarizeValues(meta.Environment), meta.EnvironmentRecorded, summarizeValues(predecessorMeta.Environment))
	}
	requireNoSelector(t, "successor metadata environment", meta.Environment, f.caller.paths())
	if meta.EffectiveConfig.Policy == nil || predecessorMeta.EffectiveConfig.Policy == nil ||
		!slices.Equal(meta.EffectiveConfig.Policy.WritableRoots, predecessorMeta.EffectiveConfig.Policy.WritableRoots) {
		t.Fatalf("successor writable roots differ from the predecessor's recorded policy (policy present: successor=%t predecessor=%t)",
			meta.EffectiveConfig.Policy != nil, predecessorMeta.EffectiveConfig.Policy != nil)
	}
	if request.Mode != predecessorRequest.Mode || meta.RuntimePreflightSHA256 != predecessorMeta.RuntimePreflightSHA256 {
		t.Fatalf("successor mode=%q capability=%q differ from predecessor mode=%q capability=%q", request.Mode, meta.RuntimePreflightSHA256, predecessorRequest.Mode, predecessorMeta.RuntimePreflightSHA256)
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

func (f *recordedEnvironmentFixture) assertReconstructedCandidates(t *testing.T) {
	t.Helper()
	_, predecessorMeta := f.loadRecords(t, f.predecessorID)
	if len(f.captures) == 0 {
		t.Fatal("the continuation never prepared the recorded provider candidate")
	}
	for index, capture := range f.captures {
		inspection := capture.candidate.Inspection
		if !slices.Equal(inspection.Environment, predecessorMeta.Environment) {
			t.Fatalf("%s candidate %d inspection environment %s want recorded %s", capture.hook, index, summarizeValues(inspection.Environment), summarizeValues(predecessorMeta.Environment))
		}
		requireNoSelector(t, capture.hook+" inspection environment", inspection.Environment, f.caller.paths())
		if !slices.Equal(capture.candidate.WritableRoots, predecessorMeta.EffectiveConfig.Policy.WritableRoots) {
			t.Fatalf("%s candidate %d writable roots %s want recorded %s", capture.hook, index, summarizeValues(capture.candidate.WritableRoots), summarizeValues(predecessorMeta.EffectiveConfig.Policy.WritableRoots))
		}
	}
}

func (f *recordedEnvironmentFixture) assertCallerEnvironmentRestored(t *testing.T) {
	t.Helper()
	for key, want := range map[string]string{
		"HOME": f.caller.home, "CODEX_HOME": f.caller.codexHome, "XDG_CONFIG_HOME": f.caller.configHome,
		"XDG_DATA_HOME": f.caller.dataHome, "TMPDIR": f.caller.tmpDir, "PATH": f.cliDir,
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("caller %s was not restored after the command", key)
		}
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
