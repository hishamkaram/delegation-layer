package app

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const providerRecheckAnswer = "principal answer"

// providerRecheckSource is one sealed historical Claude-stream predicate
// whose recorded interpretation rejects provider-native follow-up evidence
// that the current advisory revision accepts.
type providerRecheckSource struct {
	provider    string
	interactive bool
	// opaqueContent adds an unknown content block to the root follow-up
	// message. The frozen scoped revisions accept the neutral follow-up cycle
	// and reject only that block; the current advisory revisions accept both.
	opaqueContent bool
	reference     task.PredicateRef
	target        task.PredicateRef
}

// isScopedRecheckSource reports whether reference is one of the eight frozen
// scoped revisions: print native-permissions-v4 (read-only) and -v5 (write),
// interactive-v5, and CCR v5 and v5-modeled. Each was the current revision
// before the advisory wire projection and is identified by its exact digest.
func isScopedRecheckSource(reference task.PredicateRef) bool {
	switch reference.Version + "@" + reference.SHA256 {
	case "native-permissions-v4@b3c26a3eae4e39d6a68f55e191b7ac2aa7f05476103ee2d111de0fd873880f0d",
		"native-permissions-v5@27bf2e76be4185559ecaa8cfbd404d81744281298b6a29b6bf99ab4e1f4b1b29",
		"interactive-v5@9a104f6c0941b9604ce04a5fe209699de17a3e1ac659e30a58b99ec32939b7d0",
		"interactive-v5@bd63d2e508a83af6e92ccd98dee85eded7ab2ca484d44d388af3b66bc405a2d5",
		"ccr-foreground-v5@54250742f26a4f9e4fe1378d9dad1b85f5e3793d5f4ebdcf7b1ed6e3d96c3a44",
		"ccr-foreground-v5@468b9b0e25cd7d9936ef2293197ba6b72f16ac2aa9546eac70f837b7f5fdcb59",
		"ccr-foreground-v5-modeled@7483b129ea91cc745e5a45573df3669a7297b98b8bf43d2c1f28a3edae874f3c",
		"ccr-foreground-v5-modeled@e8413e812488699abb7b6c1695665927dfd39d820cb54ebe380b7cb8cdf4da61":
		return true
	default:
		return false
	}
}

// isCurrentRecheckTarget reports whether reference is one of the eight exact
// current advisory revisions: print native-permissions-v5 (read-only) and -v6
// (write), interactive-v6, and CCR v6 and v6-modeled.
func isCurrentRecheckTarget(reference task.PredicateRef) bool {
	switch reference.Version + "@" + reference.SHA256 {
	case "native-permissions-v5@a4f38219dcb1a105cf20c75f54341effde2310b6a15f61665a3b597eaa205fa8",
		"native-permissions-v6@7b64659e74a5b11e4717b5f942efa577544c2dd0730479e0f47a05ff0402bd8b",
		"interactive-v6@f06decd5719453bb8282dcb464befef53e0511e63b05ca71cf78b32bc622876c",
		"interactive-v6@2e7aeed42a66ad48127010a0591187a83a39c58a02973b1037542052020b2b95",
		"ccr-foreground-v6@7076f23f03b4560307f3543c47aa7afe25f6e7f716df9db84f2b66820eb27cdf",
		"ccr-foreground-v6@fb854fcff38fd76896c85710067517612a1cda601957818fe602b861f36615fd",
		"ccr-foreground-v6-modeled@fcc68c97680afa51aabe05f85319c27352bb267d5063cc560bcc970e2995dcaa",
		"ccr-foreground-v6-modeled@b66e115dbcaa16aa8d46fdaf6fd64b4489c287b7bc380b65c6d4f7bfd004692e":
		return true
	default:
		return false
	}
}

// TestHistoricalClaudeStreamRechecksRecoverPrincipalThroughProductionStores
// seals each eligible source in a production task store, publishes its
// original rejection, then runs the public recheck command twice.
func TestHistoricalClaudeStreamRechecksRecoverPrincipalThroughProductionStores(t *testing.T) {
	registry := NativeCatalog().Registry()
	sources := providerRecheckSources(t, registry)
	for _, source := range sources {
		t.Run(source.provider+"/"+source.reference.Mode+"/"+source.reference.Version, func(t *testing.T) {
			root, taskID := sealedProviderRecheckTask(t, registry, source)
			first := runRecheckTestCommand(t, registry, root, taskID, "recheck", 0)
			assertRecoveredResponse(t, first, source.target, providerRecheckAnswer)
			cached := runRecheckTestCommand(t, registry, root, taskID, "recheck", 0)
			if cached.Recheck == nil || cached.Recheck.AssessmentID != first.Recheck.AssessmentID || cached.Recheck.Target != source.target {
				t.Fatalf("cached recheck changed: first=%+v cached=%+v", first.Recheck, cached.Recheck)
			}
			assertRecoveredResponse(t, cached, source.target, providerRecheckAnswer)
			if collect := runRecheckTestCommand(t, registry, root, taskID, "collect", 4); collect.Publication != task.PublicationRejected.String() {
				t.Fatalf("original rejection changed: %+v", collect)
			}
		})
	}
}

func providerRecheckSources(t *testing.T, registry predicate.Registry) []providerRecheckSource {
	t.Helper()
	var sources []providerRecheckSource
	versions := map[string][]string{}
	for _, registration := range []commonprovider.Registration{claude.Registration(), ccr.Registration()} {
		for _, interpreter := range registration.Interpreters {
			reference := interpreter.Reference()
			recovery, err := registry.ResolveRecovery(reference)
			if err != nil {
				continue
			}
			interactive := strings.HasPrefix(reference.Version, "interactive-")
			sources = append(sources, providerRecheckSource{
				provider: registration.Description.ID, interactive: interactive, opaqueContent: isScopedRecheckSource(reference),
				reference: reference, target: recovery.Target(),
			})
			key := registration.Description.ID + "/" + reference.Mode
			versions[key] = append(versions[key], reference.Version)
		}
	}
	want := map[string][]string{
		"claude:print/read-only":       {"interactive-v1", "interactive-v2", "interactive-v3", "interactive-v4", "interactive-v5", "native-permissions-v1", "native-permissions-v2", "native-permissions-v3", "native-permissions-v4"},
		"claude:print/workspace-write": {"interactive-v1", "interactive-v2", "interactive-v3", "interactive-v4", "interactive-v5", "native-permissions-v2", "native-permissions-v3", "native-permissions-v4", "native-permissions-v5"},
		"ccr:print/read-only":          {"ccr-foreground-v1", "ccr-foreground-v2", "ccr-foreground-v3", "ccr-foreground-v3-modeled", "ccr-foreground-v4", "ccr-foreground-v4-modeled", "ccr-foreground-v5", "ccr-foreground-v5-modeled"},
		"ccr:print/workspace-write":    {"ccr-foreground-v1", "ccr-foreground-v2", "ccr-foreground-v3", "ccr-foreground-v3-modeled", "ccr-foreground-v4", "ccr-foreground-v4-modeled", "ccr-foreground-v5", "ccr-foreground-v5-modeled"},
	}
	for key, wanted := range want {
		got := slices.Sorted(slices.Values(versions[key]))
		if !slices.Equal(got, wanted) {
			t.Fatalf("%s eligible recovery sources=%q want=%q", key, got, wanted)
		}
	}
	scoped := 0
	for _, source := range sources {
		if !isCurrentRecheckTarget(source.target) || source.target.Adapter != source.reference.Adapter || source.target.Mode != source.reference.Mode {
			t.Fatalf("%s recovers to a non-current target %+v", source.reference.Version, source.target)
		}
		if source.opaqueContent {
			scoped++
		}
	}
	if scoped != 8 {
		t.Fatalf("%d eligible sources are frozen scoped revisions, want all 8", scoped)
	}
	return sources
}

// sealedProviderRecheckTask seals the source's recheck stream and requires
// the source to publish its original rejection.
func sealedProviderRecheckTask(t *testing.T, registry predicate.Registry, source providerRecheckSource) (string, string) {
	t.Helper()
	root, taskID, outcome := sealProviderRecheckTask(t, registry, source)
	if outcome.Verdict != task.VerdictRejected {
		t.Fatalf("source %s outcome verdict=%q diagnostics=%+v, want rejected", source.reference.Version, outcome.Verdict, outcome.Diagnostics)
	}
	return root, taskID
}

// sealProviderRecheckTask seals the source's recheck stream in a production
// task store and collects it under the source.
func sealProviderRecheckTask(t *testing.T, registry predicate.Registry, source providerRecheckSource) (string, string, *task.OutcomeRecord) {
	t.Helper()
	store, err := taskdir.InitStoreWithPredicates(filepath.Join(t.TempDir(), "state"), registry)
	if err != nil {
		t.Fatal(err)
	}
	taskID := strings.Repeat("f", 32)
	td := createProviderRecheckTask(t, store, taskID, source)
	session, err := claude.FreshSessionID(store.RootID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	writeProviderRecheckEvidence(t, td, source, providerRecheckStdout(source, session))
	if _, err = td.Seal(task.InvocationStarted, 0, "", source.reference); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	outcome, cleanupErr, collectErr := td.Collect(source.reference)
	if cleanupErr != nil || collectErr != nil || outcome == nil {
		t.Fatalf("source outcome=%+v cleanup=%v err=%v", outcome, cleanupErr, collectErr)
	}
	root := store.Root
	closeAppTestTask(t, store, td)
	return root, taskID, outcome
}

// writeProviderRecheckEvidence writes the raw stream; the runner owns the
// sealed interaction journal for interactive tasks.
func writeProviderRecheckEvidence(t *testing.T, td *taskdir.TaskDir, source providerRecheckSource, stdout string) {
	t.Helper()
	if source.interactive {
		if err := td.BeginInteraction(); err != nil {
			t.Fatal(err)
		}
	}
	if err := td.WriteRawFiles(stdout, ""); err != nil {
		t.Fatal(err)
	}
	if source.interactive {
		if err := td.SealInteraction(); err != nil {
			t.Fatal(err)
		}
	}
}

func createProviderRecheckTask(t *testing.T, store *taskdir.Store, taskID string, source providerRecheckSource) *taskdir.TaskDir {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("brief")
	requested := task.TaskConfig{Permission: source.reference.Mode, Budget: "1m0s", Interactive: source.interactive}
	if source.provider == ccr.Provider {
		requested.Model = "fixture-alias"
	}
	digest := task.ComputeSHA256([]byte("provider-recheck"))
	request := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: taskID, Provider: source.provider, Mode: source.reference.Mode, CanonicalCwd: cwd, RequestedConfig: requested, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	meta := &task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: taskID, RequestedConfig: requested, EffectiveConfig: task.EffectiveConfig{Containment: "provider-owned", Approval: "native", Digest: digest}, Containment: "provider-owned", Approval: "native", ProviderExecutable: "/tmp/provider-recheck", ProviderVersion: "fixture", PublisherBuild: "app-test", PublisherVersion: "app-test", Predicate: source.reference, SupervisorConfig: task.SupervisorRef{ClientExecutable: "/tmp/app-pueue", ClientSHA256: digest, ResolvedConfigSHA256: digest, ConfigPath: "/tmp/app-pueue.yml", ConfigDigest: digest, Endpoint: "unix:/tmp/app-pueue.sock", ObservedVersion: pueue.FixtureVersion}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	td, err := store.CreateTask(taskID, request, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	return td
}

// providerRecheckStdout is a neutral Claude stream: the caller's principal
// result, a nested agent message, then a provider-native task-notification
// cycle with repeated same-session init. Interactive sources observe it after
// input closure. Every revision before the scoped one, including the unscoped
// interactive-v4, rejects it. With opaqueContent the root follow-up message
// also carries an unknown content block, which every frozen revision including
// the scoped ones rejects; the current advisory revisions publish the
// principal answer from either stream.
func providerRecheckStdout(source providerRecheckSource, session string) string {
	permission := "plan"
	if source.reference.Mode == config.ModeWorkspaceWrite {
		permission = "bypassPermissions"
	}
	init := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":"fixture","apiKeySource":"none","cwd":"/workspace","tools":["Read"],"mcp_servers":[],"model":"fixture","permissionMode":%q}`, session, permission)
	result := `{"type":"result","subtype":"success","is_error":false,"result":%q,"stop_reason":"end_turn","session_id":%q,"terminal_reason":"completed","queued_turn_count":0,"num_turns":1%s}`
	lines := []string{init, fmt.Sprintf(result, providerRecheckAnswer, session, `,"result_index":0`)}
	if source.interactive {
		lines = append(lines, `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	}
	content := `{"type":"text","text":"neutral"}`
	if source.opaqueContent {
		content += `,{"type":"server_tool_use_v9","id":"srvtoolu_fixture","input":{}}`
	}
	lines = append(lines,
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"parent_tool_use_id":"toolu_nested_agent","message":{"content":[{"type":"text","text":"nested"}]}}`, session),
		init,
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"parent_tool_use_id":null,"message":{"content":[%s]}}`, session, content),
		fmt.Sprintf(result, "neutral", session, `,"result_index":1,"origin":{"kind":"task-notification","producer":"session-task","runId":"run-1"}`))
	return strings.Join(lines, "\n") + "\n"
}

func TestProviderRecheckFixtureKeepsSealedEvidenceImmutable(t *testing.T) {
	registry := NativeCatalog().Registry()
	sources := providerRecheckSources(t, registry)
	root, taskID := sealedProviderRecheckTask(t, registry, sources[0])
	stdoutPath := filepath.Join(root, "tasks", taskID, "raw", "stdout")
	before, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	runRecheckTestCommand(t, registry, root, taskID, "recheck", 0)
	after, err := os.ReadFile(stdoutPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("recheck changed sealed stdout: %v", err)
	}
}

// TestScopedRecheckSourcesRejectOnlyOpaqueContent keeps the frozen scoped
// semantics visible: each scoped source publishes the principal answer from
// the neutral follow-up cycle, so its recheck rejection above comes only from
// the unknown content block the current advisory revision accepts.
func TestScopedRecheckSourcesRejectOnlyOpaqueContent(t *testing.T) {
	registry := NativeCatalog().Registry()
	for _, source := range providerRecheckSources(t, registry) {
		if !source.opaqueContent {
			continue
		}
		t.Run(source.provider+"/"+source.reference.Mode+"/"+source.reference.Version, func(t *testing.T) {
			source.opaqueContent = false
			root, taskID, outcome := sealProviderRecheckTask(t, registry, source)
			if outcome.Verdict != task.VerdictCommitted {
				t.Fatalf("scoped source %+v on the neutral cycle: verdict=%q diagnostics=%+v, want committed", source.reference, outcome.Verdict, outcome.Diagnostics)
			}
			answer, err := os.ReadFile(filepath.Join(root, "tasks", taskID, outcome.Payload.Basename))
			if err != nil || string(answer) != providerRecheckAnswer {
				t.Fatalf("scoped source answer=%q err=%v, want %q", answer, err, providerRecheckAnswer)
			}
		})
	}
}
