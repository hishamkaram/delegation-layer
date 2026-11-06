package app

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These consumer tests compose the production catalog, store, admission and
// collection with deterministic ACP and run evidence. They launch no provider:
// native OpenCode behavior is proven only by Root's native acceptance runs.

// openCodeACPVersion is the current ACP output contract version, whose answer
// is the final root message run; its pinned digests are the independent
// expectation for fresh tasks and every new successor.
const openCodeACPVersion = "acp-v2"

var openCodeACPDigests = map[string]string{
	config.ModeReadOnly:       "baa34ff1bf27c2a15152509de747058d8ac00d48b373729c934c19466d84af57",
	config.ModeWorkspaceWrite: "e06654f08b1808e1cecd621efe0a7dba2e1179895d2253e754e34a14162f9214",
}

// openCodeACPV1Version is the historical ACP output contract, whose answer is
// all root text of the prompt. Its digests never change: admitted acp-v1
// records keep reconstructing and replaying under them.
const openCodeACPV1Version = "acp-v1"

var openCodeACPV1Digests = map[string]string{
	config.ModeReadOnly:       "84091a88654cbbef98c32c623559b98a510f5edc7e7efc7df47373a47a8115ca",
	config.ModeWorkspaceWrite: "10d0b4534c133b94adebcb4c4403bd25ae5af9da50732cd28d2f30712d1c1059",
}

// openCodeACPReference is the registered current ACP predicate pinned for mode.
func openCodeACPReference(t *testing.T, mode string) task.PredicateRef {
	t.Helper()
	return exactReference(t, catalogRegistration(t, openCodeProvider), mode, openCodeACPVersion, openCodeACPDigests[mode])
}

// openCodeACPV1Reference is the registered historical acp-v1 predicate pinned
// for mode.
func openCodeACPV1Reference(t *testing.T, mode string) task.PredicateRef {
	t.Helper()
	return exactReference(t, catalogRegistration(t, openCodeProvider), mode, openCodeACPV1Version, openCodeACPV1Digests[mode])
}

func openCodeAgent(mode string) string {
	if mode == config.ModeWorkspaceWrite {
		return "build"
	}
	return "plan"
}

// openCodeACPStream is ACP evidence for one task: the handshake, the opened or
// resumed session naming session, the mode acknowledgement, the principal
// answer, and optionally the terminal prompt response.
func openCodeACPStream(mode, session string, terminal bool) string {
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":%q,"configOptions":[]}}`, session),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"mode","currentValue":%q}]}}`, openCodeAgent(mode)),
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}}}}`, session, outputUpgradeAnswer),
	}
	if terminal {
		lines = append(lines, `{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`)
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestOpenCodeFreshAdmissionUsesOnlyTheACPMachineProtocol(t *testing.T) {
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			requireOpenCodeFreshAdmission(t, mode)
		})
	}
}

// requireOpenCodeFreshAdmission admits a fresh task through the production
// catalog: static `acp --help` inspection, ACP argv and predicate, one machine
// protocol factory, and refusal of an interactive request.
func requireOpenCodeFreshAdmission(t *testing.T, mode string) {
	t.Helper()
	environment := installOutputUpgradeProvider(t, "opencode")
	workspace := testsupport.CanonicalTempDir(t)
	catalog := NativeCatalog()
	request, err := admission.BuildRequest(strings.Repeat("a", 32), strings.Repeat("b", 32), openCodeProvider, workspace, task.TaskConfig{Permission: mode, Budget: "1m0s"}, []byte("brief"), nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.Candidate(request, environment)
	if err != nil {
		t.Fatal(err)
	}
	if runtime := candidate.Inspection.Runtime; !slices.Equal(runtime.HelpArgs, []string{"acp"}) || len(runtime.RequiredFlags) != 0 {
		t.Fatalf("static runtime inspection help=%q flags=%q, want help [acp] and no invented flag", runtime.HelpArgs, runtime.RequiredFlags)
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(profile.Plan.Arguments, []string{"acp"}) || !profile.Plan.Predicate.Equal(openCodeACPReference(t, mode)) || profile.Identity != nil || profile.Interaction != nil {
		t.Fatalf("fresh profile argv=%q predicate=%+v identity=%t interaction=%t", profile.Plan.Arguments, profile.Plan.Predicate, profile.Identity != nil, profile.Interaction != nil)
	}
	assertOpenCodeFreshProtocol(t, profile)
	interactive := request
	interactive.RequestedConfig.Interactive = true
	if _, err = catalog.Candidate(interactive, environment); err == nil {
		t.Fatal("an interactive OpenCode request was admitted without a native question protocol")
	}
}

// assertOpenCodeFreshProtocol opens the profile's one machine protocol with a
// fresh expectation; it starts with initialize and records no identity yet.
func assertOpenCodeFreshProtocol(t *testing.T, profile PreparedProfile) {
	t.Helper()
	calls := 0
	protocol, interaction, err := profile.OpenProtocol(task.SessionExpectation{}, func(task.SessionIdentity) error { calls++; return nil })
	if err != nil || protocol == nil || interaction != nil {
		t.Fatalf("OpenProtocol protocol=%t interaction=%t err=%v", protocol != nil, interaction != nil, err)
	}
	frames, err := protocol.Start([]byte("brief"))
	if err != nil || len(frames) != 1 || !bytes.Contains(frames[0], []byte(`"method":"initialize"`)) || calls != 0 {
		t.Fatalf("Start frames=%d err=%v identity calls=%d", len(frames), err, calls)
	}
}

// openCodeNativeV2Revision is the run dialect the seed matrix admitted before
// ACP; commonprovider.NativeProfileRevision is the released run dialect.
const openCodeNativeV2Revision = "opencode-native-v2"

// openCodeRunRevisions are the historical run dialects a stored OpenCode task
// can record, the seed matrix's first.
func openCodeRunRevisions() []string {
	return []string{openCodeNativeV2Revision, commonprovider.NativeProfileRevision}
}

// openCodeHistoricalRunProfile admits request from the record a native run
// task persisted under revision and source. The record is coherent before the
// production reconstruction runs: the run profile revision with its recorded
// approval, the recorded environment and writable roots, the source predicate,
// and the run dialect's own runtime capability. Callers persist only the
// returned admitted profile, through tasklifecycle.NewMeta.
func openCodeHistoricalRunProfile(t *testing.T, catalog commonprovider.Catalog, request task.TaskRecord, revision string, source task.PredicateRef, environment []string) PreparedProfile {
	t.Helper()
	// Every native dialect projects the same environment and writable roots,
	// so the host candidate supplies the inputs the historical task recorded.
	host, err := catalog.Candidate(request, environment)
	if err != nil {
		t.Fatalf("host candidate: %v", err)
	}
	approval := openCodeAgent(request.Mode)
	if request.Mode == config.ModeWorkspaceWrite {
		approval += ":auto"
	}
	effective, err := commonprovider.NativeEffectiveConfigForRevision(request, host.WritableRoots, host.Inspection.ExecutableSHA256, approval, revision)
	if err != nil {
		t.Fatal(err)
	}
	record := task.MetaRecord{EffectiveConfig: effective, Environment: slices.Clone(host.Inspection.Environment), EnvironmentRecorded: true, Predicate: source}
	// A record without a capability digest is not statically admissible; its
	// candidate exposes the run inspection whose digest the task recorded.
	inspection, err := catalog.ExistingCandidate(request, record, environment)
	if err != nil {
		t.Fatalf("historical run inspection: %v", err)
	}
	if record.RuntimePreflightSHA256, err = commonprovider.RuntimeCapabilityDigest(*inspection.Inspection); err != nil {
		t.Fatal(err)
	}
	existing, err := catalog.ExistingCandidate(request, record, environment)
	if err != nil {
		t.Fatalf("historical run candidate: %v", err)
	}
	profile, err := existing.Admit()
	if err != nil {
		t.Fatalf("historical run admission: %v", err)
	}
	requireExactRunReconstruction(t, profile, record)
	return profile
}

// requireExactRunReconstruction requires profile to be the run launch that
// record describes: its revision, predicate, environment, effective
// configuration and runtime capability, observed through a run identity.
func requireExactRunReconstruction(t *testing.T, profile PreparedProfile, record task.MetaRecord) {
	t.Helper()
	if profile.Plan.Arguments[0] != "run" || !profile.Plan.Predicate.Equal(record.Predicate) || profile.Identity == nil || profile.Protocol != nil ||
		profile.Effective.Policy.ProfileRevision != record.EffectiveConfig.Policy.ProfileRevision ||
		profile.RuntimePreflightSHA256 != record.RuntimePreflightSHA256 || !slices.Equal(profile.Plan.Environment, record.Environment) ||
		!task.CompareEffectiveConfigs(profile.Effective, record.EffectiveConfig) {
		t.Fatalf("historical record did not reconstruct exactly: argv=%q predicate=%+v", profile.Plan.Arguments, profile.Plan.Predicate)
	}
}

// openCodeMigrationCases are the historical OpenCode rows and their run-v5
// controls, each recorded under the run dialect revision: every run predicate
// a sealed predecessor can carry.
func openCodeMigrationCases(t *testing.T, revision string) []outputUpgradeCase {
	t.Helper()
	var cases []outputUpgradeCase
	for _, testCase := range append(outputUpgradeCases(t), outputUpgradeControlCases(t)...) {
		if testCase.provider == openCodeProvider {
			testCase.revision = revision
			cases = append(cases, testCase)
		}
	}
	if len(cases) != 8 {
		t.Fatalf("%d OpenCode run sources, want v2, v3, v4 and v5 for both modes", len(cases))
	}
	return cases
}

func closeOpenCodeStore(t *testing.T, store *taskdir.Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Error(err)
	}
}

func closeOpenCodeTask(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	if err := td.Close(); err != nil {
		t.Error(err)
	}
}
