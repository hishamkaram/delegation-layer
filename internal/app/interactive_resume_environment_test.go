package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/pi"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInteractiveDispatchResumeUsesSavedPiSessionRoot(t *testing.T) {
	prepareFailure := errors.New("actual candidate preparation failed")
	for _, testCase := range []interactivePiResumeTestCase{
		{name: "successful admission"},
		{name: "candidate preparation error", failAdmission: true},
		{name: "missing predecessor environment", missingEnvironment: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newInteractivePiResumeFixture(t, testCase, prepareFailure)
			result := dispatchInteractivePiResume(fixture)

			assertInteractivePiResumeResult(t, fixture, result)
		})
	}
}

type interactivePiResumeTestCase struct {
	name               string
	failAdmission      bool
	missingEnvironment bool
}

type interactivePiResumeFixture struct {
	testCase              interactivePiResumeTestCase
	prepareFailure        error
	callerHome            string
	callerSessionRoot     string
	savedSessionRoot      string
	savedEnvironment      []string
	root                  string
	workspace             string
	runner                string
	supervisor            *pueue.Client
	options               pueue.Options
	commandLog            string
	deps                  Dependencies
	candidateSessionRoots []string
	predecessorID         string
	sessionID             string
}

func newInteractivePiResumeFixture(t *testing.T, testCase interactivePiResumeTestCase, prepareFailure error) *interactivePiResumeFixture {
	t.Helper()
	fixture := &interactivePiResumeFixture{
		testCase:       testCase,
		prepareFailure: prepareFailure,
		root:           filepath.Join(canonicalAppTestTempDir(t), "state"),
		workspace:      canonicalAppTestTempDir(t),
		runner:         filepath.Join(canonicalAppTestTempDir(t), "delegate-run"),
		predecessorID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		sessionID:      "11111111-1111-4111-8111-111111111111",
	}
	configureInteractivePiResumeEnvironment(t, fixture)
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	fixture.supervisor, fixture.options, _, fixture.commandLog = newOrdinarySubmissionReceiptSupervisor(t)
	t.Cleanup(func() {
		if err := fixture.supervisor.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.deps = newInteractivePiResumeDependencies(t, fixture)
	prepareInteractivePiResumePredecessor(t, fixture)
	return fixture
}

func configureInteractivePiResumeEnvironment(t *testing.T, fixture *interactivePiResumeFixture) {
	t.Helper()
	savedHome := filepath.Join(canonicalAppTestTempDir(t), "saved-home")
	fixture.callerHome = filepath.Join(canonicalAppTestTempDir(t), "caller-home")
	fixture.callerSessionRoot = filepath.Join(fixture.callerHome, "pi-sessions")
	fixture.savedSessionRoot = filepath.Join(savedHome, "pi-sessions")
	for _, path := range []string{fixture.callerSessionRoot, fixture.savedSessionRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pathValue := os.Getenv("PATH")
	t.Setenv("HOME", fixture.callerHome)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", fixture.callerSessionRoot)
	fixture.savedEnvironment = []string{
		"HOME=" + savedHome,
		"PATH=" + pathValue,
		"PI_CODING_AGENT_SESSION_DIR=" + fixture.savedSessionRoot,
	}
}

func newInteractivePiResumeDependencies(t *testing.T, fixture *interactivePiResumeFixture) Dependencies {
	t.Helper()
	registration := pi.Registration()
	registration.Description.SupportedOptions = append(registration.Description.SupportedOptions, commonprovider.OptionInteractive)
	registration.Description.Interaction = task.InteractionCapabilities{
		SupportedModes: []string{config.ModeReadOnly},
		Clarification:  true,
	}
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return Dependencies{
		Catalog: catalog,
		PrepareProfile: func(request task.TaskRecord) (PreparedProfile, error) {
			sessionRoot := os.Getenv("PI_CODING_AGENT_SESSION_DIR")
			fixture.candidateSessionRoots = append(fixture.candidateSessionRoots, sessionRoot)
			if fixture.testCase.failAdmission && len(fixture.candidateSessionRoots) == 2 {
				return PreparedProfile{}, fixture.prepareFailure
			}
			profile := appTestExecutionProfile(&request, "/usr/bin/true", nil)
			profile.Plan.Predicate = pi.Reference()
			profile.Plan.Environment = []string{
				"HOME=" + os.Getenv("HOME"),
				"PATH=" + os.Getenv("PATH"),
				"PI_CODING_AGENT_SESSION_DIR=" + sessionRoot,
			}
			profile.Interaction = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
				return nil, nil
			}
			return profile, nil
		},
		SupervisorOptions:           fixture.options,
		InitialSupervisorExecutable: fixture.supervisor.Binding().ClientExecutable,
	}
}

func prepareInteractivePiResumePredecessor(t *testing.T, fixture *interactivePiResumeFixture) {
	t.Helper()
	store, err := openStore(fixture.root, fixture.deps.storeDependencies(), true)
	if err != nil {
		t.Fatal(err)
	}
	var predecessorDir *taskdir.TaskDir
	t.Cleanup(func() {
		if predecessorDir != nil {
			if closeErr := predecessorDir.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if store != nil {
			if closeErr := store.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
	brief := []byte("continue this exact Pi session")
	request, err := buildRequest(store.RootID, fixture.predecessorID, pi.Provider, fixture.workspace, task.TaskConfig{
		Permission: config.ModeReadOnly,
		Budget:     "1m0s",
	}, brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	predecessorProfile := appTestExecutionProfile(&request, "/usr/bin/true", nil)
	predecessorProfile.Plan.Predicate = pi.Reference()
	if !fixture.testCase.missingEnvironment {
		predecessorProfile.Plan.Environment = slices.Clone(fixture.savedEnvironment)
	}
	meta := newMeta(request, predecessorProfile, fixture.supervisor.Binding(), fixture.runner, task.RunnerOwnershipCustom, "interactive-resume-test")
	predecessorDir, err = store.CreateTask(fixture.predecessorID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	sealInteractivePiPredecessor(t, predecessorDir, fixture.sessionID, fixture.workspace)
	if err = predecessorDir.Close(); err != nil {
		t.Fatal(err)
	}
	predecessorDir = nil
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
}

func dispatchInteractivePiResume(fixture *interactivePiResumeFixture) commandResult {
	return dispatch(Arguments{
		Root:        fixture.root,
		Provider:    pi.Provider,
		Cwd:         fixture.workspace,
		BriefBytes:  []byte("continue the Pi session interactively"),
		ResumeTask:  fixture.predecessorID,
		PueueConfig: fixture.supervisor.Binding().ConfigPath,
		Runner:      fixture.runner,
		Config: task.TaskConfig{
			Permission:  config.ModeReadOnly,
			Budget:      "1m",
			Interactive: true,
		},
	}, fixture.deps)
}

func assertInteractivePiResumeResult(t *testing.T, fixture *interactivePiResumeFixture, result commandResult) {
	t.Helper()
	assertInteractivePiResumeCandidateRoots(t, fixture, result)
	assertInteractivePiResumeCallerEnvironment(t, fixture)
	switch {
	case fixture.testCase.missingEnvironment:
		assertInteractivePiMissingEnvironment(t, fixture, result)
	case fixture.testCase.failAdmission:
		assertInteractivePiPreparationFailure(t, fixture, result)
	default:
		assertInteractivePiSuccessfulResume(t, fixture, result)
	}
}

func assertInteractivePiResumeCandidateRoots(t *testing.T, fixture *interactivePiResumeFixture, result commandResult) {
	t.Helper()
	if fixture.testCase.missingEnvironment {
		if len(fixture.candidateSessionRoots) != 0 {
			t.Fatalf("missing predecessor environment prepared candidates with ambient state: %v", fixture.candidateSessionRoots)
		}
		return
	}
	if len(fixture.candidateSessionRoots) != 2 || fixture.candidateSessionRoots[0] != fixture.savedSessionRoot || fixture.candidateSessionRoots[1] != fixture.savedSessionRoot {
		t.Fatalf("Pi candidate session roots = %v, want both validations to use %q; dispatch result=%+v", fixture.candidateSessionRoots, fixture.savedSessionRoot, result)
	}
}

func assertInteractivePiResumeCallerEnvironment(t *testing.T, fixture *interactivePiResumeFixture) {
	t.Helper()
	if got := os.Getenv("HOME"); got != fixture.callerHome {
		t.Errorf("caller HOME after dispatch = %q, want %q", got, fixture.callerHome)
	}
	if got := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); got != fixture.callerSessionRoot {
		t.Errorf("caller Pi session root after dispatch = %q, want %q", got, fixture.callerSessionRoot)
	}
}

func assertInteractivePiMissingEnvironment(t *testing.T, fixture *interactivePiResumeFixture, result commandResult) {
	t.Helper()
	if result.code == 0 || result.err == nil || !strings.Contains(result.err.Error(), "predecessor launch environment is not recorded") {
		t.Fatalf("missing predecessor environment result = %+v, want explicit continuation failure", result)
	}
	if result.response.TaskRecord != "not_created" {
		t.Fatalf("missing predecessor environment created a successor: %+v", result.response)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
}

func assertInteractivePiPreparationFailure(t *testing.T, fixture *interactivePiResumeFixture, result commandResult) {
	t.Helper()
	if result.code == 0 || !errors.Is(result.err, fixture.prepareFailure) {
		t.Fatalf("candidate failure result = %+v, want preparation error", result)
	}
	if result.response.TaskRecord != "not_created" {
		t.Fatalf("failed candidate preparation created a successor: %+v", result.response)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
}

func assertInteractivePiSuccessfulResume(t *testing.T, fixture *interactivePiResumeFixture, result commandResult) {
	t.Helper()
	if result.code != 0 || result.err != nil || result.response.Admission != task.AdmissionAdmitted.String() {
		t.Fatalf("interactive Pi resume dispatch failed: %+v", result)
	}
	if result.response.TaskRecord != "created" || result.response.TaskID == "" {
		t.Fatalf("interactive Pi resume did not create a successor task: %+v", result.response)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
	assertInteractivePiSuccessor(t, fixture.root, fixture.deps, result.response.TaskID, fixture.sessionID, fixture.savedSessionRoot)
}

func sealInteractivePiPredecessor(t *testing.T, td *taskdir.TaskDir, sessionID, workspace string) {
	t.Helper()
	start, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordProviderIdentity(task.SessionIdentity{Provider: pi.Provider, ConversationID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles(string(interactivePiSuccessStream(sessionID, workspace)), ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", pi.Reference()); err != nil {
		t.Fatal(err)
	}
	if err = start.Release(); err != nil {
		t.Fatal(err)
	}
	outcome, cleanupErr, collectErr := td.Collect(pi.Reference())
	if cleanupErr != nil || collectErr != nil || outcome == nil {
		t.Fatalf("sealing Pi predecessor cleanup=%v collect=%v outcome=%+v", cleanupErr, collectErr, outcome)
	}
}

func interactivePiSuccessStream(sessionID, workspace string) []byte {
	message := `{"role":"assistant","content":[{"type":"text","text":"answer"}],"stopReason":"stop"}`
	lines := []string{
		fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":%q}`, sessionID, workspace),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, message),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, message),
		fmt.Sprintf(`{"type":"agent_end","messages":[%s]}`, message),
	}
	return []byte(strings.Join(lines, "\n"))
}

func assertInteractivePiSuccessor(t *testing.T, root string, deps Dependencies, id, sessionID, sessionRoot string) {
	t.Helper()
	store, err := openStore(root, deps.storeDependencies(), false)
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
	request, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || meta == nil || request.PriorSession == nil || request.PriorSession.ConversationID != sessionID {
		t.Fatalf("successor lost the exact Pi session identity: request=%+v", request)
	}
	if !slices.Contains(meta.Environment, "PI_CODING_AGENT_SESSION_DIR="+sessionRoot) {
		t.Fatalf("successor metadata did not retain the saved Pi session root: %v", meta.Environment)
	}
	if request.RequestedConfig.Interactive != true {
		t.Fatalf("successor lost interactive mode: %+v", request.RequestedConfig)
	}
}
