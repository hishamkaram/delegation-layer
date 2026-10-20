package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// The public continuation of a historical OpenCode run task is a migration,
// not an output-only upgrade: the successor is a new ACP admission that
// resumes the exact recorded session with the recorded environment and
// writable roots. Run argv, the run effective configuration and the run
// capability digest remain historical assertions, not migration requirements.

// openCodeOtherSession is a well-formed session the predecessor never recorded.
const openCodeOtherSession = "ses_01J9K7P2E5S8V1Z3B2C4D6F8G1"

const openCodeACPInitialize = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}}`

// TestOpenCodeRunPredecessorContinuesThroughACP creates exact-session
// successors of sealed run predecessors under v2, v3, v4 and v5, in both modes,
// through both public entry points. The seed matrix records the
// opencode-native-v2 dialect; the released native-permissions-v1 dialect is
// additional coverage of the same migration.
func TestOpenCodeRunPredecessorContinuesThroughACP(t *testing.T) {
	for _, revision := range openCodeRunRevisions() {
		for _, testCase := range openCodeMigrationCases(t, revision) {
			for _, entry := range outputUpgradeEntryPoints() {
				t.Run(revision+"/"+testCase.mode+"/v"+testCase.source.Version+"/"+entry.name, func(t *testing.T) {
					runOpenCodeACPSuccessor(t, newOutputUpgradeFixture(t, testCase), entry)
				})
			}
		}
	}
}

func runOpenCodeACPSuccessor(t *testing.T, f *outputUpgradeFixture, entry outputUpgradeEntryPoint) {
	t.Helper()
	before := snapshotOutputUpgradeTask(t, f.root, outputUpgradePredecessorID)
	result := entry.run(f)
	if result.code != 0 || result.err != nil || result.response.TaskRecord != "created" || result.response.TaskID == "" {
		t.Fatalf("%s did not create a successor: code=%d err=%v", entry.name, result.code, result.err)
	}
	assertOrdinarySubmissionCommandCount(t, f.commandLog, "add", 1)
	request, meta := readOutputUpgradeTask(t, f, result.response.TaskID)
	assertOutputUpgradeSuccessorIdentity(t, f, request, meta)
	if !meta.Predicate.Equal(openCodeACPReference(t, f.testCase.mode)) || meta.EffectiveConfig.Policy == nil || meta.EffectiveConfig.Policy.ProfileRevision != "opencode-acp-v1" {
		t.Fatalf("successor predicate=%+v, want the ACP contract", meta.Predicate)
	}
	if !slices.Equal(meta.Environment, f.meta.Environment) || !slices.Equal(meta.EffectiveConfig.Policy.WritableRoots, f.meta.EffectiveConfig.Policy.WritableRoots) {
		t.Fatalf("successor environment (%s) or writable roots differ from the predecessor", summarizeValues(meta.Environment))
	}
	assertOpenCodeACPSuccessorLaunch(t, f, request, meta)
	assertOpenCodeACPSuccessorCollection(t, f, result.response.TaskID)
	if after := snapshotOutputUpgradeTask(t, f.root, outputUpgradePredecessorID); !equalOutputUpgradeSnapshots(before, after) {
		t.Error("predecessor bytes changed while creating its successor")
	}
	assertOpenCodeRunPredecessorRebuilds(t, f)
}

// assertOpenCodeRunPredecessorRebuilds requires the stored predecessor to keep
// its recorded run dialect and source predicate through reconstruction.
func assertOpenCodeRunPredecessorRebuilds(t *testing.T, f *outputUpgradeFixture) {
	t.Helper()
	request, meta := readOutputUpgradeTask(t, f, outputUpgradePredecessorID)
	if meta.EffectiveConfig.Policy == nil || meta.EffectiveConfig.Policy.ProfileRevision != f.testCase.revision {
		t.Fatalf("predecessor was not recorded under the %s run dialect", f.testCase.revision)
	}
	rebuilt, err := prepareMatchedProfile(f.deps, f.root, request, meta)
	if err != nil || rebuilt.Plan.Arguments[0] != "run" || !rebuilt.Plan.Predicate.Equal(f.testCase.source) || rebuilt.Effective.Policy.ProfileRevision != f.testCase.revision {
		t.Fatalf("predecessor no longer rebuilds its run contract: err=%v", err)
	}
}

// assertOpenCodeACPSuccessorLaunch rebuilds the persisted successor through
// the production matcher. Its protocol refuses a fresh-session expectation,
// and a refused or mismatched resume fails without opening a new session.
func assertOpenCodeACPSuccessorLaunch(t *testing.T, f *outputUpgradeFixture, request task.TaskRecord, meta task.MetaRecord) {
	t.Helper()
	profile, err := prepareMatchedProfile(f.deps, f.root, request, meta)
	if err != nil {
		t.Fatalf("persisted successor does not rebuild: %v", err)
	}
	if !slices.Equal(profile.Plan.Arguments, []string{"acp"}) || !slices.Equal(profile.Plan.Environment, f.meta.Environment) || profile.Identity != nil {
		t.Fatalf("successor launch argv=%q identity observer=%t", profile.Plan.Arguments, profile.Identity != nil)
	}
	if _, _, err = profile.OpenProtocol(task.SessionExpectation{}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("successor protocol accepted a fresh-session expectation")
	}
	assertOpenCodeResumeFailures(t, profile, request)
}

// assertOpenCodeResumeFailures drives the successor protocol through each
// refused resume. None records an identity or sends session/new.
func assertOpenCodeResumeFailures(t *testing.T, profile PreparedProfile, request task.TaskRecord) {
	t.Helper()
	noResume := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{}}}`
	if update, recorded, err := openCodeSuccessorReceive(t, profile, request, noResume); err == nil || len(update.Outbound) != 0 || *recorded != 0 {
		t.Errorf("an agent without session/resume was accepted: frames=%d err=%v", len(update.Outbound), err)
	}
	for name, line := range map[string]string{
		"another session": `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"` + openCodeOtherSession + `","configOptions":[]}}`,
		"refused resume":  `{"jsonrpc":"2.0","id":2,"error":{"code":-32002,"message":"session not found"}}`,
	} {
		update, recorded, err := openCodeSuccessorReceive(t, profile, request, openCodeACPInitialize, line)
		opened := slices.ContainsFunc(update.Outbound, func(frame []byte) bool { return bytes.Contains(frame, []byte(`"session/new"`)) })
		switch {
		case opened || *recorded != 0:
			t.Errorf("%s: opened a new session=%t, identity records=%d", name, opened, *recorded)
		case name == "another session" && !errors.Is(err, task.ErrIdentityMismatch):
			t.Errorf("%s: err=%v, want %v", name, err, task.ErrIdentityMismatch)
		case name == "refused resume" && (err != nil || !update.Done || len(update.Outbound) != 0):
			t.Errorf("%s: err=%v done=%t frames=%d, want the turn ended", name, err, update.Done, len(update.Outbound))
		}
	}
}

// openCodeSuccessorReceive opens a new successor protocol with the canonical
// expectation, starts it, and feeds lines in order. It returns the last update
// and the first error, with the number of recorded identities.
func openCodeSuccessorReceive(t *testing.T, profile PreparedProfile, request task.TaskRecord, lines ...string) (execution.InteractionUpdate, *int, error) {
	t.Helper()
	recorded := new(int)
	protocol, _, err := profile.OpenProtocol(continuationExpectation(&request), func(task.SessionIdentity) error { *recorded++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = protocol.Start([]byte("follow-up brief")); err != nil {
		t.Fatal(err)
	}
	var update execution.InteractionUpdate
	for _, line := range lines {
		if update, err = protocol.Receive([]byte(line)); err != nil {
			return update, recorded, err
		}
	}
	return update, recorded, nil
}

// openCodeACPRun is what the persisted successor's own protocol did with one
// native stdout, observed through the task directory.
type openCodeACPRun struct {
	// methods are the outbound request methods in order.
	methods []string
	// resumed reports a session/resume naming exactly the recorded session.
	resumed bool
	// recorded counts the protocol's identity callbacks.
	recorded    int
	receiveErr  error
	completeErr error
	// identity is the persisted provider identity, nil when none was recorded.
	identity *task.ProviderRefRecord
	outcome  *task.OutcomeRecord
	answer   string
}

// runOpenCodeACPSuccessorTask executes the persisted successor as the runner
// does, without a process: the rebuilt profile's own protocol is opened with
// the canonical expectation and the task directory's identity recorder, and
// its turn on the canonical seal path receives stdout line by line. The same
// stdout is then sealed under the persisted predicate and collected. Nothing
// else records the identity.
func runOpenCodeACPSuccessorTask(t *testing.T, f *outputUpgradeFixture, id, stdout string) openCodeACPRun {
	t.Helper()
	request, meta := readOutputUpgradeTask(t, f, id)
	profile, err := prepareMatchedProfile(f.deps, f.root, request, meta)
	if err != nil {
		t.Fatalf("persisted successor does not rebuild: %v", err)
	}
	store, err := openStore(f.root, f.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeStore(t, store)
	td, err := store.OpenTask(id)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeTask(t, td)
	var run openCodeACPRun
	protocol, _, err := profile.OpenProtocol(continuationExpectation(&request), func(identity task.SessionIdentity) error {
		run.recorded++
		return td.RecordProviderIdentity(identity)
	})
	if err != nil {
		t.Fatal(err)
	}
	run.outcome = sealOutputUpgradeTurn(t, td, meta.Predicate, stdout, func() error {
		return run.drive(protocol, f.session, stdout)
	})
	// An absent record is reported with its not-exist error; only that is
	// an observation rather than a failure.
	identity, absent, err := td.ReadProviderIdentityPresence()
	if err != nil && !absent {
		t.Fatal(err)
	}
	run.identity = identity
	if run.outcome.Verdict == task.VerdictCommitted {
		answer, readErr := os.ReadFile(filepath.Join(td.Dir, run.outcome.Payload.Basename))
		if readErr != nil {
			t.Fatal(readErr)
		}
		run.answer = string(answer)
	}
	return run
}

// drive starts protocol and feeds each stdout line until the first refusal;
// a stream the protocol accepted entirely is then completed. Refusals are
// observations of the run; only a failure to start or to read an outbound
// frame is an error.
func (r *openCodeACPRun) drive(protocol execution.Protocol, session, stdout string) error {
	frames, err := protocol.Start([]byte("follow-up brief"))
	if err != nil {
		return err
	}
	if err = r.observe(frames, session); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		var update execution.InteractionUpdate
		update, r.receiveErr = protocol.Receive([]byte(line))
		if err = r.observe(update.Outbound, session); err != nil {
			return err
		}
		if r.receiveErr != nil {
			break
		}
	}
	if r.receiveErr == nil {
		r.completeErr = protocol.Complete()
	}
	return nil
}

func (r *openCodeACPRun) observe(frames [][]byte, session string) error {
	for _, frame := range frames {
		var request struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(frame, &request); err != nil {
			return fmt.Errorf("outbound frame: %w", err)
		}
		if request.Method != "" {
			r.methods = append(r.methods, request.Method)
		}
		if bytes.Contains(frame, []byte(`"method":"session/resume","params":{"sessionId":"`+session+`"`)) {
			r.resumed = true
		}
	}
	return nil
}

// openCodeACPResumedTurn is the request sequence of one resumed turn.
var openCodeACPResumedTurn = []string{"initialize", "session/resume", "session/set_config_option", "session/prompt"}

// assertOpenCodeACPSuccessorCollection runs the successor's own protocol over
// a complete resumed turn: the exact session is persisted once by the
// protocol's recorder, and collection publishes the principal answer.
func assertOpenCodeACPSuccessorCollection(t *testing.T, f *outputUpgradeFixture, id string) {
	t.Helper()
	run := runOpenCodeACPSuccessorTask(t, f, id, openCodeACPStream(f.testCase.mode, f.session, true))
	if !slices.Equal(run.methods, openCodeACPResumedTurn) || !run.resumed || run.receiveErr != nil || run.completeErr != nil {
		t.Fatalf("successor protocol methods=%q resumed=%t receive=%v complete=%v", run.methods, run.resumed, run.receiveErr, run.completeErr)
	}
	if run.recorded != 1 || run.identity == nil || run.identity.Provider != openCodeProvider || run.identity.ConversationID != f.session {
		t.Fatalf("successor protocol recorded %d identities, persisted identity present=%t", run.recorded, run.identity != nil)
	}
	if run.outcome.Verdict != task.VerdictCommitted || run.answer != outputUpgradeAnswer {
		t.Fatalf("successor collection: verdict=%q answer=%q diagnostics=%+v", run.outcome.Verdict, run.answer, run.outcome.Diagnostics)
	}
}

// TestOpenCodeACPSuccessorCollectionRequiresTheResumedTurn runs the
// successor's own protocol over evidence that never completed the resumed
// turn: a partial EOF, a refused resume, and a resume of another session.
// None opens a new session, only the exact session is ever persisted, and
// exit 0, a valid seal and answer-shaped text do not publish success.
func TestOpenCodeACPSuccessorCollectionRequiresTheResumedTurn(t *testing.T) {
	for _, testCase := range openCodeMigrationCases(t, openCodeNativeV2Revision) {
		if testCase.source.Version != "4" {
			continue
		}
		for name, failure := range openCodeACPResumeFailures(testCase.mode) {
			t.Run(testCase.mode+"/"+name, func(t *testing.T) {
				f := newOutputUpgradeFixture(t, testCase)
				result := outputUpgradeEntryPoints()[0].run(f)
				if result.err != nil || result.response.TaskID == "" {
					t.Fatalf("successor was not created: %v", result.err)
				}
				run := runOpenCodeACPSuccessorTask(t, f, result.response.TaskID, failure.stream)
				failure.assert(t, f, run)
			})
		}
	}
}

// openCodeACPResumeFailure is one unfinished resumed turn and the protocol
// observation it requires.
type openCodeACPResumeFailure struct {
	stream string
	// methods and recorded are the exact requests sent and identities recorded.
	methods  []string
	recorded int
	// mismatch requires the protocol to refuse the stream as another session.
	mismatch bool
}

func openCodeACPResumeFailures(mode string) map[string]openCodeACPResumeFailure {
	return map[string]openCodeACPResumeFailure{
		"partial EOF": {stream: openCodeACPStream(mode, outputUpgradeOpenCodeSession, false), methods: openCodeACPResumedTurn, recorded: 1},
		"refused resume": {
			stream:  openCodeACPInitialize + "\n" + `{"jsonrpc":"2.0","id":2,"error":{"code":-32002,"message":"session not found"}}` + "\n",
			methods: openCodeACPResumedTurn[:2],
		},
		"another session": {stream: openCodeACPStream(mode, openCodeOtherSession, true), methods: openCodeACPResumedTurn[:2], mismatch: true},
	}
}

func (failure openCodeACPResumeFailure) assert(t *testing.T, f *outputUpgradeFixture, run openCodeACPRun) {
	t.Helper()
	if !slices.Equal(run.methods, failure.methods) || !run.resumed || run.recorded != failure.recorded {
		t.Fatalf("protocol methods=%q resumed=%t identities=%d, want %q and %d", run.methods, run.resumed, run.recorded, failure.methods, failure.recorded)
	}
	if (run.identity != nil) != (failure.recorded == 1) || (run.identity != nil && run.identity.ConversationID != f.session) {
		t.Fatalf("persisted identity present=%t, want only the exact recorded session", run.identity != nil)
	}
	if failure.mismatch != errors.Is(run.receiveErr, task.ErrIdentityMismatch) || (!failure.mismatch && (run.receiveErr != nil || run.completeErr == nil)) {
		t.Fatalf("protocol receive=%v complete=%v, want the turn refused", run.receiveErr, run.completeErr)
	}
	if run.outcome.Verdict != task.VerdictRejected || run.answer != "" || len(run.outcome.Diagnostics) == 0 {
		t.Fatalf("verdict=%q diagnostics=%+v, want rejected", run.outcome.Verdict, run.outcome.Diagnostics)
	}
}

// TestOpenCodeRunSuccessorPreparesItsOwnACPRecord prepares the successor of
// each historical run record through the production catalog. It must be the
// ACP preparation of the recorded inputs, and the record it persists must
// reconstruct to the identical preparation.
func TestOpenCodeRunSuccessorPreparesItsOwnACPRecord(t *testing.T) {
	for _, revision := range openCodeRunRevisions() {
		for _, testCase := range openCodeMigrationCases(t, revision) {
			t.Run(revision+"/"+testCase.mode+"/v"+testCase.source.Version, func(t *testing.T) {
				requireOpenCodeSuccessorOwnRecord(t, testCase)
			})
		}
	}
}

// requireOpenCodeSuccessorOwnRecord prepares the successor of testCase's
// historical record and reconstructs the ACP record that successor persists.
func requireOpenCodeSuccessorOwnRecord(t *testing.T, testCase outputUpgradeCase) {
	t.Helper()
	installOutputUpgradeProvider(t, testCase.executable())
	request := outputUpgradeSuccessorRequest(t, testCase)
	catalog := NativeCatalog()
	predecessor := outputUpgradeStoredRecord(t, catalog, testCase, request, testCase.source)
	candidate, err := catalog.SuccessorCandidate(request, predecessor)
	if err != nil {
		t.Fatal(err)
	}
	profile := outputUpgradeAdmit(t, testCase, candidate)
	if !slices.Equal(profile.Plan.Arguments, []string{"acp"}) || !profile.Plan.Predicate.Equal(openCodeACPReference(t, testCase.mode)) ||
		!slices.Equal(profile.Plan.Environment, predecessor.Environment) || !slices.Equal(profile.WritableRoots, predecessor.EffectiveConfig.Policy.WritableRoots) {
		t.Fatalf("successor argv=%q predicate=%+v is not the ACP preparation of the recorded inputs", profile.Plan.Arguments, profile.Plan.Predicate)
	}
	successor := outputUpgradePreparationOf(t, candidate, profile)
	own := tasklifecycle.NewMeta(request, profile, task.SupervisorRef{}, "/runner", task.RunnerOwnershipManaged, outputUpgradeBuild)
	rebuilt, err := catalog.ExistingCandidate(request, own)
	if err != nil {
		t.Fatal(err)
	}
	if again := outputUpgradePreparationOf(t, rebuilt, outputUpgradeAdmit(t, testCase, rebuilt)); !reflect.DeepEqual(successor, again) {
		t.Errorf("successor record reconstructs differently in: %s", strings.Join(differingPreparationComponents(successor, again), ", "))
	}
}
