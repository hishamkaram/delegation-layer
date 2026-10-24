package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// An admitted acp-v1 ACP task keeps acp-v1 for reconstruction and replay,
// where its answer is all root text of the prompt. Its new exact-session
// successor resumes the recorded session under the current acp-v2 contract,
// whose answer is the final root message run.

const (
	openCodeACPProfileRevision = "opencode-acp-v1"
	// openCodeACPPreamble is the root message that precedes a tool call in
	// the final-run stream; only acp-v1 publishes it.
	openCodeACPPreamble = "I'll read the acceptance marker file."
)

// openCodeACPFreshTurn is the request sequence of one fresh ACP turn.
var openCodeACPFreshTurn = []string{"initialize", "session/new", "session/set_config_option", "session/prompt"}

// openCodeACPFinalRunStream is one complete ACP turn whose root answer has
// distinct messages: a preamble message, a read tool call, then the final
// message carrying the principal answer in two chunks. The opened session
// line serves both session/new and session/resume.
func openCodeACPFinalRunStream(mode, session string) string {
	update := func(body string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":%s}}`, session, body)
	}
	chunk := func(id, text string) string {
		return update(fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","messageId":%q,"content":{"type":"text","text":%q}}`, id, text))
	}
	answer, split := outputUpgradeAnswer, len(outputUpgradeAnswer)/2
	lines := []string{
		openCodeACPInitialize,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":%q,"configOptions":[]}}`, session),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"mode","currentValue":%q}]}}`, openCodeAgent(mode)),
		chunk("msg_preamble", openCodeACPPreamble),
		update(`{"sessionUpdate":"tool_call","toolCallId":"call_read","title":"read","kind":"read","status":"pending"}`),
		update(`{"sessionUpdate":"tool_call_update","toolCallId":"call_read","status":"completed"}`),
		chunk("msg_final", answer[:split]),
		chunk("msg_final", answer[split:]),
		`{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

// newOpenCodeACPV1Fixture is the shared output-upgrade fixture around a
// sealed, committed acp-v1 ACP predecessor instead of a historical run
// record.
func newOpenCodeACPV1Fixture(t *testing.T, mode string) *outputUpgradeFixture {
	t.Helper()
	f := initOutputUpgradeFixture(t, outputUpgradeCase{
		name: "opencode " + mode + " acp-v1", provider: openCodeProvider, mode: mode,
		source: openCodeACPV1Reference(t, mode), target: openCodeACPReference(t, mode), revision: openCodeACPProfileRevision,
	})
	createOpenCodeACPV1Predecessor(t, f)
	return f
}

// createOpenCodeACPV1Predecessor admits the predecessor under acp-v1, runs
// its fresh turn through its own protocol on the canonical seal path, and
// requires collection to publish the acp-v1 whole-turn answer.
func createOpenCodeACPV1Predecessor(t *testing.T, f *outputUpgradeFixture) {
	t.Helper()
	store, err := taskdir.InitStoreWithPredicates(f.root, f.deps.Catalog.Registry())
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeStore(t, store)
	brief := []byte("principal brief")
	if f.request, err = admission.BuildRequest(store.RootID, outputUpgradePredecessorID, openCodeProvider, f.workspace, f.testCase.requestedConfig(), brief, nil); err != nil {
		t.Fatal(err)
	}
	profile := openCodeACPV1Profile(t, f)
	f.meta = tasklifecycle.NewMeta(f.request, profile, f.supervisor.Binding(), f.runner, task.RunnerOwnershipManaged, outputUpgradeBuild)
	td, err := store.CreateTask(outputUpgradePredecessorID, &f.request, brief, &f.meta)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeTask(t, td)
	f.session = outputUpgradeOpenCodeSession
	var run openCodeACPRun
	protocol, _, err := profile.OpenProtocol(task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		run.recorded++
		return td.RecordProviderIdentity(identity)
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout := openCodeACPFinalRunStream(f.testCase.mode, f.session)
	outcome := sealOutputUpgradeTurn(t, td, f.meta.Predicate, stdout, func() error { return run.drive(protocol, f.session, stdout) })
	if !slices.Equal(run.methods, openCodeACPFreshTurn) || run.receiveErr != nil || run.completeErr != nil || run.recorded != 1 {
		t.Fatalf("predecessor protocol methods=%q receive=%v complete=%v identities=%d", run.methods, run.receiveErr, run.completeErr, run.recorded)
	}
	requireOpenCodeACPV1Answer(t, td, outcome)
}

// openCodeACPV1Profile reconstructs the stored acp-v1 record through the
// production catalog's stored-predicate preparation of a fresh ACP admission.
func openCodeACPV1Profile(t *testing.T, f *outputUpgradeFixture) PreparedProfile {
	t.Helper()
	fresh, err := f.deps.Catalog.Candidate(f.request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	current, err := fresh.Admit()
	if err != nil {
		t.Fatal(err)
	}
	seed := tasklifecycle.NewMeta(f.request, current, f.supervisor.Binding(), f.runner, task.RunnerOwnershipManaged, outputUpgradeBuild)
	seed.Predicate = f.testCase.source
	existing, err := f.deps.Catalog.ExistingCandidate(f.request, seed, os.Environ())
	if err != nil {
		t.Fatalf("stored acp-v1 candidate: %v", err)
	}
	profile, err := existing.Admit()
	if err != nil {
		t.Fatalf("stored acp-v1 admission: %v", err)
	}
	if !profile.Plan.Predicate.Equal(f.testCase.source) || !slices.Equal(profile.Plan.Arguments, []string{"acp"}) || profile.Protocol == nil ||
		profile.Identity != nil || !slices.Equal(profile.Plan.Environment, current.Plan.Environment) || !task.CompareEffectiveConfigs(profile.Effective, current.Effective) {
		t.Fatalf("acp-v1 record did not reconstruct as the ACP launch with its own predicate: argv=%q predicate=%+v", profile.Plan.Arguments, profile.Plan.Predicate)
	}
	return profile
}

// requireOpenCodeACPV1Answer requires the acp-v1 whole-turn answer: the
// preamble message followed by the final message.
func requireOpenCodeACPV1Answer(t *testing.T, td *taskdir.TaskDir, outcome *task.OutcomeRecord) {
	t.Helper()
	if outcome.Verdict != task.VerdictCommitted {
		t.Fatalf("acp-v1 predecessor verdict=%q diagnostics=%+v", outcome.Verdict, outcome.Diagnostics)
	}
	answer, err := os.ReadFile(filepath.Join(td.Dir, outcome.Payload.Basename))
	if err != nil || string(answer) != openCodeACPPreamble+outputUpgradeAnswer {
		t.Fatalf("acp-v1 predecessor answer=%q err=%v, want the whole-turn answer", answer, err)
	}
}

// TestOpenCodeACPV1PredecessorContinuesUnderACPV2 creates the exact-session
// successor of a sealed acp-v1 predecessor in both modes through both public
// entry points.
func TestOpenCodeACPV1PredecessorContinuesUnderACPV2(t *testing.T) {
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		for _, entry := range outputUpgradeEntryPoints() {
			t.Run(mode+"/"+entry.name, func(t *testing.T) {
				runOpenCodeACPV1Successor(t, newOpenCodeACPV1Fixture(t, mode), entry)
			})
		}
	}
}

func runOpenCodeACPV1Successor(t *testing.T, f *outputUpgradeFixture, entry outputUpgradeEntryPoint) {
	t.Helper()
	before := snapshotOutputUpgradeTask(t, f.root, outputUpgradePredecessorID)
	result := entry.run(f)
	if result.code != 0 || result.err != nil || result.response.TaskRecord != "created" || result.response.TaskID == "" {
		t.Fatalf("%s did not create a successor: code=%d err=%v", entry.name, result.code, result.err)
	}
	assertOrdinarySubmissionCommandCount(t, f.commandLog, "add", 1)
	request, meta := readOutputUpgradeTask(t, f, result.response.TaskID)
	assertOutputUpgradeSuccessorIdentity(t, f, request, meta)
	if !meta.Predicate.Equal(f.testCase.target) || meta.EffectiveConfig.Policy == nil || meta.EffectiveConfig.Policy.ProfileRevision != openCodeACPProfileRevision {
		t.Fatalf("successor predicate=%+v, want the acp-v2 contract", meta.Predicate)
	}
	if !slices.Equal(meta.Environment, f.meta.Environment) || !task.CompareEffectiveConfigs(meta.EffectiveConfig, f.meta.EffectiveConfig) ||
		meta.RuntimePreflightSHA256 != f.meta.RuntimePreflightSHA256 {
		t.Fatalf("successor launch contract differs from the acp-v1 predecessor (environment %s)", summarizeValues(meta.Environment))
	}
	if rebuilt, err := prepareMatchedProfile(f.deps, f.root, request, meta); err != nil || !rebuilt.Plan.Predicate.Equal(f.testCase.target) {
		t.Fatalf("persisted successor does not rebuild under acp-v2: err=%v", err)
	}
	assertOpenCodeACPSuccessorLaunch(t, f, request, meta)
	assertOpenCodeACPV2FinalRunCollection(t, f, result.response.TaskID)
	assertOpenCodeACPV1PredecessorRebuilds(t, f)
	if after := snapshotOutputUpgradeTask(t, f.root, outputUpgradePredecessorID); !equalOutputUpgradeSnapshots(before, after) {
		t.Error("predecessor bytes changed while creating or collecting its successor")
	}
}

// assertOpenCodeACPV2FinalRunCollection runs the successor's own protocol
// over a resumed turn with distinct root messages; acp-v2 publishes only the
// final message.
func assertOpenCodeACPV2FinalRunCollection(t *testing.T, f *outputUpgradeFixture, id string) {
	t.Helper()
	run := runOpenCodeACPSuccessorTask(t, f, id, openCodeACPFinalRunStream(f.testCase.mode, f.session))
	if !slices.Equal(run.methods, openCodeACPResumedTurn) || !run.resumed || run.receiveErr != nil || run.completeErr != nil {
		t.Fatalf("successor protocol methods=%q resumed=%t receive=%v complete=%v", run.methods, run.resumed, run.receiveErr, run.completeErr)
	}
	if run.recorded != 1 || run.identity == nil || run.identity.ConversationID != f.session {
		t.Fatalf("successor recorded %d identities, persisted identity present=%t", run.recorded, run.identity != nil)
	}
	if run.outcome.Verdict != task.VerdictCommitted || run.answer != outputUpgradeAnswer || len(run.outcome.Diagnostics) != 0 {
		t.Fatalf("successor collection: verdict=%q answer=%q diagnostics=%+v, want only the final message", run.outcome.Verdict, run.answer, run.outcome.Diagnostics)
	}
}

// assertOpenCodeACPV1PredecessorRebuilds requires the stored predecessor to
// rebuild under acp-v1 and its collection to keep the whole-turn answer,
// while collection under the current contract is refused.
func assertOpenCodeACPV1PredecessorRebuilds(t *testing.T, f *outputUpgradeFixture) {
	t.Helper()
	request, meta := readOutputUpgradeTask(t, f, outputUpgradePredecessorID)
	rebuilt, err := prepareMatchedProfile(f.deps, f.root, request, meta)
	if err != nil || !meta.Predicate.Equal(f.testCase.source) || !rebuilt.Plan.Predicate.Equal(f.testCase.source) || !slices.Equal(rebuilt.Plan.Arguments, []string{"acp"}) {
		t.Fatalf("predecessor no longer rebuilds its acp-v1 contract: err=%v", err)
	}
	store, err := openStore(f.root, f.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeStore(t, store)
	td, err := store.OpenTask(outputUpgradePredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeTask(t, td)
	outcome, cleanupErr, collectErr := td.Collect(f.testCase.source)
	if err = errors.Join(cleanupErr, collectErr); err != nil || outcome == nil {
		t.Fatalf("predecessor collection under acp-v1: %v", err)
	}
	requireOpenCodeACPV1Answer(t, td, outcome)
	if _, cleanupErr, collectErr = td.Collect(f.testCase.target); cleanupErr != nil || !errors.Is(collectErr, task.ErrIncompatiblePredicate) {
		t.Fatalf("predecessor collection under acp-v2 cleanup=%v error=%v, want %v", cleanupErr, collectErr, task.ErrIncompatiblePredicate)
	}
}

// TestOpenCodeACPSuccessorRefusesAnUnknownACPPredicate keeps the exact
// historical guard: an ACP record whose predicate is not exactly a registered
// ACP revision of its mode refuses its successor with the reconstruction's
// own error.
func TestOpenCodeACPSuccessorRefusesAnUnknownACPPredicate(t *testing.T) {
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			f := newOpenCodeACPV1Fixture(t, mode)
			altered := f.testCase.source
			altered.SHA256 = strings.Repeat("1", len(altered.SHA256))
			other := config.ModeWorkspaceWrite
			if mode == other {
				other = config.ModeReadOnly
			}
			for name, reference := range map[string]task.PredicateRef{"altered acp-v1 digest": altered, "other mode acp-v1": openCodeACPV1Reference(t, other)} {
				record := f.meta
				record.Predicate = reference
				_, existingErr := f.deps.Catalog.ExistingCandidate(f.request, record, os.Environ())
				_, successorErr := f.deps.Catalog.SuccessorCandidate(f.request, record, os.Environ())
				if existingErr == nil || successorErr == nil || existingErr.Error() != successorErr.Error() {
					t.Fatalf("%s: successor error=%v, reconstruction error=%v, want the same refusal", name, successorErr, existingErr)
				}
			}
		})
	}
}
