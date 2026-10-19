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
// whose recorded single-result interpretation rejects provider-native
// follow-up evidence that the current attributed revision accepts.
type providerRecheckSource struct {
	provider    string
	interactive bool
	reference   task.PredicateRef
	target      task.PredicateRef
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
			sources = append(sources, providerRecheckSource{provider: registration.Description.ID, interactive: interactive, reference: reference, target: recovery.Target()})
			key := registration.Description.ID + "/" + reference.Mode
			versions[key] = append(versions[key], reference.Version)
		}
	}
	want := map[string][]string{
		"claude:print/read-only":       {"interactive-v1", "interactive-v2", "interactive-v3", "interactive-v4", "native-permissions-v1", "native-permissions-v2", "native-permissions-v3"},
		"claude:print/workspace-write": {"interactive-v1", "interactive-v2", "interactive-v3", "interactive-v4", "native-permissions-v2", "native-permissions-v3", "native-permissions-v4"},
		"ccr:print/read-only":          {"ccr-foreground-v1", "ccr-foreground-v2", "ccr-foreground-v3", "ccr-foreground-v3-modeled", "ccr-foreground-v4", "ccr-foreground-v4-modeled"},
		"ccr:print/workspace-write":    {"ccr-foreground-v1", "ccr-foreground-v2", "ccr-foreground-v3", "ccr-foreground-v3-modeled", "ccr-foreground-v4", "ccr-foreground-v4-modeled"},
	}
	for key, wanted := range want {
		got := slices.Sorted(slices.Values(versions[key]))
		if !slices.Equal(got, wanted) {
			t.Fatalf("%s eligible recovery sources=%q want=%q", key, got, wanted)
		}
	}
	for _, source := range sources {
		if !strings.HasSuffix(source.target.Version, "interactive-v5") && !slices.Contains([]string{"native-permissions-v4", "native-permissions-v5", "ccr-foreground-v5", "ccr-foreground-v5-modeled"}, source.target.Version) {
			t.Fatalf("%s recovers to a non-attributed target %+v", source.reference.Version, source.target)
		}
	}
	return sources
}

func sealedProviderRecheckTask(t *testing.T, registry predicate.Registry, source providerRecheckSource) (string, string) {
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
	if cleanupErr != nil || collectErr != nil || outcome == nil || outcome.Verdict != task.VerdictRejected {
		t.Fatalf("source outcome=%+v cleanup=%v err=%v", outcome, cleanupErr, collectErr)
	}
	root := store.Root
	closeAppTestTask(t, store, td)
	return root, taskID
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
// input closure. Every historical revision, including the unscoped
// interactive-v4, rejects it; the current scoped revisions publish the
// principal answer.
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
	lines = append(lines,
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"parent_tool_use_id":"toolu_nested_agent","message":{"content":[{"type":"text","text":"nested"}]}}`, session),
		init,
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"neutral"}]}}`, session),
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
