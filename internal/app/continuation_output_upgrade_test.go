package app

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/antigravity"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

const (
	// openCodeProvider is OpenCode's registered provider ID. Its registration
	// comes from the production catalog, so these tests import no OpenCode
	// package.
	openCodeProvider    = "opencode:run"
	outputUpgradeAnswer = "principal answer"
	// The OpenCode and agy sessions are fixed provider-shaped identifiers; the
	// Claude family derives its session from the root and task identity.
	outputUpgradeOpenCodeSession = "ses_01J9K7P2E5S8V1Z3B2C4D6F8G0"
	outputUpgradeAGYConversation = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	// outputUpgradeAGYAuthFailure is agy's exact structured authentication
	// envelope. Only the current record revision classifies it.
	outputUpgradeAGYAuthFailure = `{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed or timed out"}`
)

// outputUpgradeCase is one eligible exact source predicate. A new successor
// created from a predecessor sealed under source must carry the compatible
// current output predicate target; the predecessor itself stays under source.
type outputUpgradeCase struct {
	name     string
	provider string
	mode     string
	source   task.PredicateRef
	target   task.PredicateRef
	// route is the CCR launch-help projection recorded for the source: true
	// keeps --resume-route, false keeps the explicit model and provider-only auth.
	route bool
	// authFailure selects the successor evidence whose only revision-dependent
	// result is the classification of agy's authentication envelope.
	authFailure bool
	// revision is the OpenCode run dialect the predecessor record carries.
	revision string
}

func (c outputUpgradeCase) executable() string {
	switch c.provider {
	case ccr.Provider:
		return "ccr"
	case openCodeProvider:
		return "opencode"
	case antigravity.Provider:
		return "agy"
	default:
		return "claude"
	}
}

// resumeFlag is the provider's exact-session launch argument.
func (c outputUpgradeCase) resumeFlag() string {
	switch c.provider {
	case antigravity.Provider:
		return "--conversation"
	default:
		return "--resume"
	}
}

// freshSession is the exact provider session the predecessor records.
func (c outputUpgradeCase) freshSession(rootID, taskID string) (string, error) {
	switch c.provider {
	case openCodeProvider:
		return outputUpgradeOpenCodeSession, nil
	case antigravity.Provider:
		return outputUpgradeAGYConversation, nil
	default:
		return claude.FreshSessionID(rootID, taskID)
	}
}

func (c outputUpgradeCase) catalog(t *testing.T) commonprovider.Catalog {
	t.Helper()
	if c.provider == ccr.Provider {
		return outputUpgradeCCRCatalog(t, c.route)
	}
	return NativeCatalog()
}

func (c outputUpgradeCase) requestedConfig() task.TaskConfig {
	config := task.TaskConfig{Permission: c.mode, Budget: "1m0s"}
	if c.provider == ccr.Provider {
		config.Model = "fixture-alias"
	}
	return config
}

// predecessorProfile admits the predecessor through the production catalog's
// stored-predicate preparation, so its launch contract is the one a real task
// recorded under source. OpenCode's fresh admission is ACP, so its sources are
// reconstructed from a coherent historical run record instead.
func (c outputUpgradeCase) predecessorProfile(t *testing.T, f *outputUpgradeFixture) PreparedProfile {
	t.Helper()
	if c.provider == openCodeProvider {
		return openCodeHistoricalRunProfile(t, f.deps.Catalog, f.request, c.revision, c.source, f.deps.Environment)
	}
	fresh, err := f.deps.Catalog.Candidate(f.request, f.deps.Environment)
	if err != nil {
		t.Fatalf("current candidate: %v", err)
	}
	freshProfile, err := fresh.Admit()
	if err != nil {
		t.Fatalf("current admission: %v", err)
	}
	seed := tasklifecycle.NewMeta(f.request, freshProfile, f.supervisor.Binding(), f.runner, task.RunnerOwnershipManaged, outputUpgradeBuild)
	seed.Predicate = c.source
	existing, err := f.deps.Catalog.ExistingCandidate(f.request, seed, f.deps.Environment)
	if err != nil {
		t.Fatalf("stored-predicate candidate: %v", err)
	}
	profile, err := existing.Admit()
	if err != nil {
		t.Fatalf("stored-predicate admission: %v", err)
	}
	return profile
}

// predecessorStream is the plain answer every eligible source publishes; it
// carries none of the evidence its successor's current revision newly accepts.
func (c outputUpgradeCase) predecessorStream(session string) string {
	switch c.provider {
	case openCodeProvider:
		return openCodeUpgradeStream(session, false)
	case antigravity.Provider:
		return fmt.Sprintf(`{"response":%q,"conversation_id":%q,"status":"SUCCESS"}`, outputUpgradeAnswer, session) + "\n"
	default:
		return strings.Join([]string{c.initLine(session), c.resultLine(session, outputUpgradeAnswer, "")}, "\n") + "\n"
	}
}

// openCodeUpgradeStream is one answered step. With repeatedStart it opens the
// step twice before any content, which only the current revision ignores.
func openCodeUpgradeStream(session string, repeatedStart bool) string {
	start := fmt.Sprintf(`{"type":"step_start","sessionID":%q,"part":{"type":"step-start"}}`, session)
	lines := []string{start}
	if repeatedStart {
		lines = append(lines, start)
	}
	lines = append(lines,
		fmt.Sprintf(`{"type":"text","sessionID":%q,"part":{"type":"text","text":%q}}`, session, outputUpgradeAnswer),
		fmt.Sprintf(`{"type":"step_finish","sessionID":%q,"part":{"type":"step-finish","reason":"stop","tokens":{},"cost":0}}`, session))
	return strings.Join(lines, "\n") + "\n"
}

func (c outputUpgradeCase) initLine(session string) string {
	permission := "plan"
	if c.mode == config.ModeWorkspaceWrite {
		permission = "bypassPermissions"
	}
	return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":"fixture","apiKeySource":"none","cwd":"/workspace","tools":["Read"],"mcp_servers":[],"model":"fixture","permissionMode":%q}`, session, permission)
}

func (c outputUpgradeCase) resultLine(session, text, attribution string) string {
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"stop_reason":"end_turn","session_id":%q,"terminal_reason":"completed","queued_turn_count":0,"num_turns":1%s}`, text, session, attribution)
}

// successorStream is evidence that only the current output predicate accepts:
// the principal answer followed by a nested-agent message and a task
// notification cycle whose root message carries an unknown content block
// (Claude, CCR), an empty repeated step start (OpenCode), or invalid optional
// usage metadata (agy). The frozen scoped Claude and CCR sources accept the
// cycle without the block, so the block is what only their target accepts.
func (c outputUpgradeCase) successorStream(session string) string {
	switch c.provider {
	case openCodeProvider:
		return openCodeUpgradeStream(session, true)
	case antigravity.Provider:
		if c.authFailure {
			return outputUpgradeAGYAuthFailure
		}
		return fmt.Sprintf(`{"response":%q,"conversation_id":%q,"usage":{"input_tokens":"unavailable"},"status":"SUCCESS"}`, outputUpgradeAnswer, session) + "\n"
	default:
		return providerRecheckStdout(providerRecheckSource{reference: c.target, opaqueContent: true}, session)
	}
}

// successorAnswer is the exact payload the current target publishes from
// successorStream. The Claude-stream targets frame every accepted root answer;
// the independent oracle derives it from the stream, not from the adapter.
func (c outputUpgradeCase) successorAnswer(t *testing.T, session string) string {
	t.Helper()
	switch c.provider {
	case claude.Provider, ccr.Provider:
		return expectedCurrentClaudeStreamAnswer(t, c.successorStream(session))
	default:
		return outputUpgradeAnswer
	}
}

func outputUpgradeCases(t *testing.T) []outputUpgradeCase {
	t.Helper()
	readTarget := claude.ReferenceForMode(config.ModeReadOnly)
	writeTarget := claude.ReferenceForMode(config.ModeWorkspaceWrite)
	cases := []outputUpgradeCase{
		{name: "claude read-only native-permissions-v3", provider: claude.Provider, mode: config.ModeReadOnly, source: exactReference(t, claude.Registration(), config.ModeReadOnly, "native-permissions-v3", "f1c0aa32cf4b089214f2ac727a5510dde46c923066cccfffb2a912069af3c455"), target: readTarget},
		{name: "claude read-only retained native-permissions-v4", provider: claude.Provider, mode: config.ModeReadOnly, source: exactReference(t, claude.Registration(), config.ModeReadOnly, "native-permissions-v4", "f7c1053edce279071b9caa46f727cf196d5998fc05d07afdf9c5378acd9eb25c"), target: readTarget},
		{name: "claude write native-permissions-v4", provider: claude.Provider, mode: config.ModeWorkspaceWrite, source: exactReference(t, claude.Registration(), config.ModeWorkspaceWrite, "native-permissions-v4", "dc2c8fb5cef5a1acc568e890705c28add52596398f731b0142284ef176d2e58a"), target: writeTarget},
		{name: "claude write retained native-permissions-v5", provider: claude.Provider, mode: config.ModeWorkspaceWrite, source: exactReference(t, claude.Registration(), config.ModeWorkspaceWrite, "native-permissions-v5", "333a689a864dfb03ecd4a6eace97bcf6964fea96768ddbbb86a92d98c4a7a76b"), target: writeTarget},
		{name: "claude read-only scoped native-permissions-v4", provider: claude.Provider, mode: config.ModeReadOnly, source: exactReference(t, claude.Registration(), config.ModeReadOnly, "native-permissions-v4", "b3c26a3eae4e39d6a68f55e191b7ac2aa7f05476103ee2d111de0fd873880f0d"), target: readTarget},
		{name: "claude write scoped native-permissions-v5", provider: claude.Provider, mode: config.ModeWorkspaceWrite, source: exactReference(t, claude.Registration(), config.ModeWorkspaceWrite, "native-permissions-v5", "27bf2e76be4185559ecaa8cfbd404d81744281298b6a29b6bf99ab4e1f4b1b29"), target: writeTarget},
	}
	cases = append(cases, ccrOutputUpgradeCases(t)...)
	cases = append(cases, openCodeOutputUpgradeCases(t)...)
	return append(cases, agyOutputUpgradeCases(t)...)
}

// openCodeOutputUpgradeCases pins the native OpenCode revisions 2, 3, and 4 and
// their current revision 5 for both modes, recorded under the opencode-native-v2
// run dialect. Revision 1 is a different plan and probe contract and is never a
// source.
func openCodeOutputUpgradeCases(t *testing.T) []outputUpgradeCase {
	t.Helper()
	registration := catalogRegistration(t, openCodeProvider)
	type digests struct{ v2, v3, v4, v5 string }
	byMode := map[string]digests{
		config.ModeReadOnly: {
			"b097c597256043d5776a6fdd7ff526b76cfa19ec058b15ac5160aa54fb1d7352", "4fce81c253212816c6a775938cd2194ad8cac29bfe69ae77a78ae025b170c518",
			"b375397104da62801651c8ad3c2c9f20d6144436264b276b47ae94e2287d7c20", "a5908a2a500a4fc4203b6b16d403e3b2f350380645ee38b4fcdd445ea03b1a3f",
		},
		config.ModeWorkspaceWrite: {
			"08226cfd57f8c882b5866813c849fb06bd802c82d94c992b8e540407e606b147", "7b39a6f70ed5dd6d4a2605ebf5ebe7dc81f4cf7a10e7a95cc5d807baa7666ca6",
			"1bdc5fea3de999a79475b080da565e5c74807f1e7d1b44e944002036772e58c9", "da63dc56abdbe64127c204b3e3afddb3f41f7b9ce12d2d6b2a7c397cee41c8be",
		},
	}
	var cases []outputUpgradeCase
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		sum := byMode[mode]
		target := exactReference(t, registration, mode, "5", sum.v5)
		for _, source := range []struct{ version, digest string }{{"2", sum.v2}, {"3", sum.v3}, {"4", sum.v4}} {
			cases = append(cases, outputUpgradeCase{
				name: "opencode " + mode + " v" + source.version, provider: openCodeProvider, mode: mode, revision: openCodeNativeV2Revision,
				source: exactReference(t, registration, mode, source.version, source.digest), target: target,
			})
		}
	}
	return cases
}

// agyOutputUpgradeCases pins agy's native auth2, auth3, and record-v1 sources
// and the current record-v2 target for both modes. The unproven 1.2.2 base
// revision is never a source.
func agyOutputUpgradeCases(t *testing.T) []outputUpgradeCase {
	t.Helper()
	registration := catalogRegistration(t, antigravity.Provider)
	const (
		auth2   = "53f1b5368023953447088ecbd4fc36c1b431b0fb376cf76511ed70d943d8138a"
		auth3   = "66f07cc66de3106c3a0869b28c3ce315ecae53062f6a3b76db9b12eaa076a698"
		record1 = "5497017cbe63b0a4181b1b764523f30b9e0ae0230d79c15852c945e9fbfef4b7"
		record2 = "54713d2c06eef693c09d8f08419f6dd2fd4f4802742e71d113df0dfbf1f9c43f"
	)
	var cases []outputUpgradeCase
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		target := exactReference(t, registration, mode, "print-record-v2", record2)
		for _, source := range []struct{ version, digest string }{{"1.2.2-auth2", auth2}, {"1.2.2-auth3", auth3}, {"print-record-v1", record1}} {
			cases = append(cases, outputUpgradeCase{
				name: "agy " + mode + " " + source.version, provider: antigravity.Provider, mode: mode,
				source: exactReference(t, registration, mode, source.version, source.digest), target: target,
				authFailure: source.version == "print-record-v1",
			})
		}
	}
	return cases
}

// ccrOutputUpgradeCases pins the exact CCR v4 and frozen scoped v5 sources:
// route-preserving and modeled, each read-only and write, with their current
// follow-up v7 targets. The v5 revisions were the targets before the advisory
// wire projection; they are now frozen sources of the same launch shape.
func ccrOutputUpgradeCases(t *testing.T) []outputUpgradeCase {
	t.Helper()
	registration := ccr.Registration()
	type revision struct{ version, digest string }
	type spec struct {
		mode    string
		route   bool
		sources []revision
		target  revision
	}
	var cases []outputUpgradeCase
	for _, s := range []spec{
		{config.ModeReadOnly, true, []revision{
			{"ccr-foreground-v4", "5c13f0e77406ce86e146532d163c5f6da74264743b1bdb6ed1ffc0f70a79f502"},
			{"ccr-foreground-v5", "54250742f26a4f9e4fe1378d9dad1b85f5e3793d5f4ebdcf7b1ed6e3d96c3a44"},
		}, revision{"ccr-foreground-v7", "7447125836ee96e2d8d16760d4dcb90c78d619bf75ef50f6343a012ffaf213cb"}},
		{config.ModeWorkspaceWrite, true, []revision{
			{"ccr-foreground-v4", "4816a7344a169112b9b969b1339f9b418498cbc6955816d83a046fed419d89c0"},
			{"ccr-foreground-v5", "468b9b0e25cd7d9936ef2293197ba6b72f16ac2aa9546eac70f837b7f5fdcb59"},
		}, revision{"ccr-foreground-v7", "568f56184cec02f9a853099eaf2a4b3b827baec1df97af735013d02b53ae6a6d"}},
		{config.ModeReadOnly, false, []revision{
			{"ccr-foreground-v4-modeled", "6051bed3c949dbf49eab6ced92226a5922f662b2c1c7053b72fbec3982fbdeb3"},
			{"ccr-foreground-v5-modeled", "7483b129ea91cc745e5a45573df3669a7297b98b8bf43d2c1f28a3edae874f3c"},
		}, revision{"ccr-foreground-v7-modeled", "92e13e9d0405eb9b097a78289d9e5e3b017c62f54e48dec8cb1627840732c647"}},
		{config.ModeWorkspaceWrite, false, []revision{
			{"ccr-foreground-v4-modeled", "e6d76ecf0ae8bf014713301c1ddc395481158cb7edb71d7a53e207cac79964e3"},
			{"ccr-foreground-v5-modeled", "e8413e812488699abb7b6c1695665927dfd39d820cb54ebe380b7cb8cdf4da61"},
		}, revision{"ccr-foreground-v7-modeled", "e61c42768544e0e79fa71a31cf5594ae31d2ad4115aa18b33c7584956b32f32d"}},
	} {
		target := exactReference(t, registration, s.mode, s.target.version, s.target.digest)
		for _, source := range s.sources {
			cases = append(cases, outputUpgradeCase{
				name: "ccr " + s.mode + " " + source.version, provider: ccr.Provider, mode: s.mode, route: s.route,
				source: exactReference(t, registration, s.mode, source.version, source.digest), target: target,
			})
		}
	}
	return cases
}

func catalogRegistration(t *testing.T, id string) commonprovider.Registration {
	t.Helper()
	registration, err := NativeCatalog().Lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	return registration
}

// exactReference returns the registered interpreter reference with the exact
// mode, version, and digest; a version label alone is never a selector.
func exactReference(t *testing.T, registration commonprovider.Registration, mode, version, digest string) task.PredicateRef {
	t.Helper()
	for _, interpreter := range registration.Interpreters {
		if reference := interpreter.Reference(); reference.Mode == mode && reference.Version == version && reference.SHA256 == digest {
			return reference
		}
	}
	t.Fatalf("%s %s %s %s is not registered", registration.Description.ID, mode, version, digest)
	return task.PredicateRef{}
}

type outputUpgradeEntryPoint struct {
	name string
	run  func(*outputUpgradeFixture) commandResult
}

func outputUpgradeEntryPoints() []outputUpgradeEntryPoint {
	return []outputUpgradeEntryPoint{
		{name: "continue --task", run: func(f *outputUpgradeFixture) commandResult {
			return continueTask(Arguments{Root: f.root, TaskID: outputUpgradePredecessorID, PueueConfig: f.supervisor.Binding().ConfigPath}, f.deps)
		}},
		{name: "dispatch --resume-task", run: func(f *outputUpgradeFixture) commandResult {
			return dispatch(Arguments{
				Root: f.root, Provider: f.testCase.provider, Cwd: f.workspace, BriefBytes: []byte("follow-up brief"),
				ResumeTask: outputUpgradePredecessorID, PueueConfig: f.supervisor.Binding().ConfigPath,
				Config: f.testCase.requestedConfig(),
			}, f.deps)
		}},
	}
}

// sameWireOutputUpgradeCases keeps the cases whose public continuation keeps
// the predecessor's launch dialect. The OpenCode rows are historical run
// records: reconstruction, recheck and the frozen-predecessor tests use them,
// while their public continuation migrates to ACP and is owned by
// opencode_acp_continuation_test.go.
func sameWireOutputUpgradeCases(cases []outputUpgradeCase) []outputUpgradeCase {
	return slices.DeleteFunc(cases, func(testCase outputUpgradeCase) bool { return testCase.provider == openCodeProvider })
}

func TestContinuationSuccessorsReceiveCurrentOutputPredicate(t *testing.T) {
	for _, testCase := range sameWireOutputUpgradeCases(outputUpgradeCases(t)) {
		for _, entry := range outputUpgradeEntryPoints() {
			t.Run(testCase.name+"/"+entry.name, func(t *testing.T) {
				t.Parallel()
				runOutputUpgradeScenario(t, testCase, entry)
			})
		}
	}
}

// TestContinuationOfCurrentPredicatePublishesItsCurrentAnswer runs the same
// public scenario for predecessors already admitted under each current target.
// It proves the scenario's success path is satisfiable, so the source cases
// above fail only if the successor keeps the old predicate.
func TestContinuationOfCurrentPredicatePublishesItsCurrentAnswer(t *testing.T) {
	for _, testCase := range sameWireOutputUpgradeCases(outputUpgradeControlCases(t)) {
		for _, entry := range outputUpgradeEntryPoints() {
			t.Run(testCase.name+"/"+entry.name, func(t *testing.T) {
				t.Parallel()
				runOutputUpgradeScenario(t, testCase, entry)
			})
		}
	}
}

// outputUpgradeControlCases is one case per distinct current target whose
// source is that target itself.
func outputUpgradeControlCases(t *testing.T) []outputUpgradeCase {
	t.Helper()
	var controls []outputUpgradeCase
	seen := map[task.PredicateRef]bool{}
	for _, testCase := range outputUpgradeCases(t) {
		if seen[testCase.target] {
			continue
		}
		seen[testCase.target] = true
		testCase.name = "current " + testCase.provider + " " + testCase.mode + " " + testCase.target.Version
		testCase.source = testCase.target
		controls = append(controls, testCase)
	}
	return controls
}

func runOutputUpgradeScenario(t *testing.T, testCase outputUpgradeCase, entry outputUpgradeEntryPoint) {
	t.Helper()
	fixture := newOutputUpgradeFixture(t, testCase)
	before := snapshotOutputUpgradeTask(t, fixture.root, outputUpgradePredecessorID)
	result := entry.run(fixture)
	if result.code != 0 || result.err != nil || result.response.TaskRecord != "created" || result.response.TaskID == "" {
		t.Fatalf("%s did not create a successor: code=%d err=%v task_record=%q has_task_id=%t", entry.name, result.code, result.err, result.response.TaskRecord, result.response.TaskID != "")
	}
	request, meta := readOutputUpgradeTask(t, fixture, result.response.TaskID)
	assertOutputUpgradeSuccessorIdentity(t, fixture, request, meta)
	if !meta.Predicate.Equal(testCase.target) {
		t.Errorf("successor persisted %+v, want the current compatible predicate %+v", meta.Predicate, testCase.target)
	}
	assertOutputUpgradeSuccessorCollection(t, fixture, result.response.TaskID, request, meta)
	if after := snapshotOutputUpgradeTask(t, fixture.root, outputUpgradePredecessorID); !equalOutputUpgradeSnapshots(before, after) {
		t.Error("predecessor bytes changed while creating its successor")
	}
}

func readOutputUpgradeTask(t *testing.T, f *outputUpgradeFixture, id string) (task.TaskRecord, task.MetaRecord) {
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
	request, meta, err := td.PreparedRecords()
	if err != nil || request == nil || meta == nil {
		t.Fatalf("successor records: request present=%t meta present=%t err=%v", request != nil, meta != nil, err)
	}
	return *request, *meta
}

func assertOutputUpgradeSuccessorIdentity(t *testing.T, f *outputUpgradeFixture, request task.TaskRecord, meta task.MetaRecord) {
	t.Helper()
	wantPrior := task.PriorSession{Provider: f.testCase.provider, ConversationID: f.session, PredecessorTaskID: outputUpgradePredecessorID}
	if request.PriorSession == nil || *request.PriorSession != wantPrior {
		t.Errorf("successor prior session=%+v, want %+v", request.PriorSession, wantPrior)
	}
	if request.Mode != f.testCase.mode || request.RequestedConfig.Permission != f.testCase.mode {
		t.Errorf("successor permission mode=%q config=%+v, want %q", request.Mode, request.RequestedConfig, f.testCase.mode)
	}
	if meta.RuntimePreflightSHA256 == "" || !meta.EnvironmentRecorded {
		t.Errorf("successor lost the recorded native launch contract: preflight=%q recorded=%t", meta.RuntimePreflightSHA256, meta.EnvironmentRecorded)
	}
}

// assertOutputUpgradeSuccessorCollection rebuilds the persisted successor
// through the production matcher, seals native follow-up evidence under the
// persisted predicate, and collects it.
func assertOutputUpgradeSuccessorCollection(t *testing.T, f *outputUpgradeFixture, id string, request task.TaskRecord, meta task.MetaRecord) {
	t.Helper()
	profile, err := prepareMatchedProfile(f.deps, f.root, request, meta)
	if err != nil {
		t.Fatalf("persisted successor does not rebuild: %v", err)
	}
	if !profile.Plan.Predicate.Equal(f.testCase.target) {
		t.Errorf("rebuilt successor predicate %+v, want %+v", profile.Plan.Predicate, f.testCase.target)
	}
	assertOutputUpgradeLaunch(t, f, profile, meta)
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
	outcome := sealOutputUpgradeTask(t, td, f.testCase.provider, f.session, meta.Predicate, f.testCase.successorStream(f.session))
	if f.testCase.authFailure {
		assertOutputUpgradeAuthenticationClassified(t, meta.Predicate, outcome)
		return
	}
	if outcome.Verdict != task.VerdictCommitted {
		t.Errorf("successor collection under %+v: verdict=%q diagnostics=%+v, want the committed current answer", meta.Predicate, outcome.Verdict, outcome.Diagnostics)
		return
	}
	want := f.testCase.successorAnswer(t, f.session)
	answer, err := os.ReadFile(filepath.Join(td.Dir, outcome.Payload.Basename))
	if err != nil || string(answer) != want {
		t.Errorf("successor answer=%q err=%v, want %q", answer, err, want)
	}
}

// assertOutputUpgradeAuthenticationClassified requires agy's authentication
// envelope to be rejected under the stable authentication-unavailable class,
// which only the current record revision assigns.
func assertOutputUpgradeAuthenticationClassified(t *testing.T, reference task.PredicateRef, outcome *task.OutcomeRecord) {
	t.Helper()
	if outcome.Verdict != task.VerdictRejected || len(outcome.Diagnostics) != 1 || outcome.Diagnostics[0].Code != "authentication-unavailable" {
		t.Errorf("authentication envelope under %+v: verdict=%q diagnostics=%+v, want rejected authentication-unavailable", reference, outcome.Verdict, outcome.Diagnostics)
	}
}

// assertOutputUpgradeLaunch requires the successor to keep the predecessor's
// exact native launch: session resume, recorded environment, writable roots,
// effective configuration, runtime capability, and the CCR route or modeled
// flag shape.
func assertOutputUpgradeLaunch(t *testing.T, f *outputUpgradeFixture, profile PreparedProfile, meta task.MetaRecord) {
	t.Helper()
	arguments := profile.Plan.Arguments
	if index := slices.Index(arguments, f.testCase.resumeFlag()); index < 0 || index+1 >= len(arguments) || arguments[index+1] != f.session {
		t.Errorf("successor does not resume the exact session with %s (%d launch arguments)", f.testCase.resumeFlag(), len(arguments))
	}
	if !slices.Equal(profile.Plan.Environment, f.meta.Environment) || !slices.Equal(meta.Environment, f.meta.Environment) {
		t.Errorf("successor environment changed: plan matches=%t (%s), meta matches=%t (%s), predecessor %s", slices.Equal(profile.Plan.Environment, f.meta.Environment), summarizeValues(profile.Plan.Environment), slices.Equal(meta.Environment, f.meta.Environment), summarizeValues(meta.Environment), summarizeValues(f.meta.Environment))
	}
	if !slices.Equal(profile.WritableRoots, f.meta.EffectiveConfig.Policy.WritableRoots) {
		t.Errorf("successor writable roots differ from the predecessor: %s vs %s", summarizeValues(profile.WritableRoots), summarizeValues(f.meta.EffectiveConfig.Policy.WritableRoots))
	}
	if !task.CompareEffectiveConfigs(meta.EffectiveConfig, f.meta.EffectiveConfig) || !task.CompareEffectiveConfigs(profile.Effective, f.meta.EffectiveConfig) {
		t.Errorf("successor effective configuration changed: meta matches=%t, plan matches=%t", task.CompareEffectiveConfigs(meta.EffectiveConfig, f.meta.EffectiveConfig), task.CompareEffectiveConfigs(profile.Effective, f.meta.EffectiveConfig))
	}
	if meta.RuntimePreflightSHA256 != f.meta.RuntimePreflightSHA256 || meta.ProviderExecutable != f.meta.ProviderExecutable {
		t.Errorf("successor runtime contract changed: preflight matches=%t, executable matches=%t",
			meta.RuntimePreflightSHA256 == f.meta.RuntimePreflightSHA256, meta.ProviderExecutable == f.meta.ProviderExecutable)
	}
	if f.testCase.provider == ccr.Provider {
		assertOutputUpgradeCCRLaunch(t, f.testCase.route, arguments)
	}
}

func assertOutputUpgradeCCRLaunch(t *testing.T, route bool, arguments []string) {
	t.Helper()
	routePrefix := []string{"launch", "--resume-route"}
	modeledPrefix := []string{"launch", "--model", "fixture-alias", "--auth-mode", "provider-only"}
	switch {
	case route && (!slices.Equal(arguments[:min(len(arguments), 2)], routePrefix) || slices.Contains(arguments, "--model")):
		t.Errorf("route-preserving successor lost --resume-route or forwarded a model (%d launch arguments)", len(arguments))
	case !route && !slices.Equal(arguments[:min(len(arguments), 5)], modeledPrefix):
		t.Errorf("modeled successor lost its explicit model and provider-only auth (%d launch arguments)", len(arguments))
	}
}

type outputUpgradeSnapshot map[string]string

func snapshotOutputUpgradeTask(t *testing.T, root, id string) outputUpgradeSnapshot {
	t.Helper()
	snapshot := outputUpgradeSnapshot{}
	base := filepath.Join(root, "tasks", id)
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		snapshot[strings.TrimPrefix(path, base)] = task.ComputeSHA256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func equalOutputUpgradeSnapshots(a, b outputUpgradeSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for path, digest := range a {
		if b[path] != digest {
			return false
		}
	}
	return true
}
