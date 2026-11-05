package supervisor

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func routedMetadataSource(t *testing.T, fixture parentMigrationFixture, saved task.SupervisorRef) infrastructure.SavedBindingSource {
	t.Helper()
	return routedMetadataSourceForTask(t, fixture, saved, strings.Repeat("a", 32))
}

func routedMetadataSourceForTask(t *testing.T, fixture parentMigrationFixture, saved task.SupervisorRef, taskID string) infrastructure.SavedBindingSource {
	t.Helper()
	request := supervisorfixture.NewInspectionRequest(t, fixture.store)
	request.TaskID = taskID
	digest := task.ComputeSHA256([]byte("continuation routing fixture"))
	meta := task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID,
		RequestedConfig: request.RequestedConfig, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment: "fixture-only", Approval: "never", ProviderExecutable: saved.ClientExecutable, ProviderVersion: "fixture-v2",
		PublisherBuild: "routing-fixture", PublisherVersion: "routing-fixture", Predicate: task.FixturePredicateRef(),
		SupervisorConfig: saved, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := fixture.store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := NewSavedBindingSource(request.RootID, infrastructure.SavedBindingTaskMetadata, request.TaskID, "", saved)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestReadTaskMetadataSupervisorRejectsChangedSubjectWithoutWrites(t *testing.T) {
	fixture := newSavedControlFixture(t)
	source := routedMetadataSource(t, fixture, fixture.target)
	for _, name := range []string{"kind", "root", "digest", "owner", "canceled"} {
		t.Run(name, func(t *testing.T) {
			altered, ctx := source, t.Context()
			want := infrastructure.ErrIdentity
			switch name {
			case "kind":
				altered.Kind = infrastructure.SavedBindingTaskSubmission
			case "root":
				altered.RootID = strings.Repeat("b", 32)
			case "digest":
				altered.BindingSHA256 = task.ComputeSHA256([]byte("forged metadata binding"))
			case "owner":
				altered.OwnerID = strings.Repeat("c", 32)
				want = os.ErrNotExist
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, want = canceled, context.Canceled
			}
			before := parentMigrationTree(t, fixture.store.Root)
			_, err := ReadTaskMetadataSupervisor(ctx, fixture.store, altered)
			if !errors.Is(err, want) {
				t.Fatalf("changed metadata source accepted: %v", err)
			}
			if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
				t.Fatal("validating metadata read changed target")
			}
		})
	}
}

// These immutable generation records model a settled component boundary. The
// finite status scripts and records grant no native daemon or resident authority.
func newWarmContinuationFixture(t *testing.T) (parentMigrationFixture, infrastructure.SavedBindingSource) {
	t.Helper()
	fixture := newSavedControlFixture(t)
	fixture.legacy = parentMigrationPair(t, fixture, "predecessor")
	config, err := os.ReadFile(fixture.legacy.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.legacy.ConfigDigest = task.ComputeSHA256(append(config, []byte("\n# predecessor configuration\n")...))
	base := filepath.Dir(fixture.target.ConfigPath)
	for _, name := range []string{"data", "run"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "bootstrap.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeContinuationGeneration(t, fixture, []parentMigrationRecordFixture{{task.SchemaVersion, parentMigrationIdentity(fixture.legacy), parentMigrationIdentity(fixture.target), 12345, true}})
	return fixture, routedMetadataSource(t, fixture, fixture.legacy)
}

func writeContinuationFixtureRecord[T any](t *testing.T, path string, record T) {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeContinuationGeneration(t *testing.T, fixture parentMigrationFixture, history []parentMigrationRecordFixture) {
	t.Helper()
	base := filepath.Dir(fixture.target.ConfigPath)
	writeContinuationFixtureRecord(t, filepath.Join(base, "migration-history.json"), struct {
		SchemaVersion int                            `json:"schema_version"`
		Migrations    []parentMigrationRecordFixture `json:"migrations"`
	}{task.SchemaVersion, history})
	writeContinuationFixtureRecord(t, filepath.Join(base, "supervisor.identity.json"), struct {
		SchemaVersion int                `json:"schema_version"`
		Binding       task.SupervisorRef `json:"binding"`
	}{task.SchemaVersion, fixture.target})
	writeContinuationFixtureRecord(t, filepath.Join(base, "daemon.identity.json"), parentMigrationIdentity(fixture.target))
}

func TestControlContinuationWarmMappedGenerationNeedsNoRoute(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options}).PrepareContinuation(t.Context(), fixture.store, source)
	if client != nil {
		defer func() {
			if closeErr := client.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if err != nil || client == nil {
		t.Fatalf("warm mapped continuation required owner contact: %v", err)
	}
	if client.Binding() != fixture.target || client.RuntimeBinding() != fixture.target {
		t.Fatal("continuation returned original metadata instead of verified active binding")
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("warm continuation changed durable state")
	}
}

func TestControlContinuationColdUsesOriginalMetadataAndNoFallback(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	fixture.options.Environment = append(fixture.options.Environment, "REFERENCED_UNAVAILABLE=1")
	unknown := errors.New("continuation outcome unknown")
	calls := 0
	routing := selectedControlRouting{continuation: func(_ context.Context, actual *taskdir.Store, _ infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if actual != fixture.store || observed != source || observed.ValidateBinding(fixture.legacy) != nil || observed.ValidateBinding(fixture.target) == nil {
			t.Fatal("derived generation replaced original metadata source")
		}
		return infrastructure.Disposition{}, unknown
	}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Routing: routing}).PrepareContinuation(t.Context(), fixture.store, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("unknown outcome returned ready client")))
	}
	if !errors.Is(err, unknown) || calls != 1 {
		t.Fatalf("cold continuation changed intent or retried: calls=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("cold continuation performed local preparation")
	}
}

func writeContinuationAlias(t *testing.T, fixture parentMigrationFixture, binding task.SupervisorRef) {
	t.Helper()
	base := filepath.Dir(binding.ConfigPath)
	config, err := os.ReadFile(binding.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ConfigDigest != task.ComputeSHA256(config) {
		config = append(config, []byte("\n# predecessor configuration\n")...)
	}
	path := filepath.Join(base, "legacy-pueue.yml")
	if err = os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
	writeContinuationFixtureRecord(t, filepath.Join(base, "legacy-supervisor.json"), struct {
		SchemaVersion int                `json:"schema_version"`
		Supervisor    task.SupervisorRef `json:"supervisor"`
		ConfigPath    string             `json:"config_path"`
		ConfigSHA256  string             `json:"config_sha256"`
	}{task.SchemaVersion, binding, path, task.ComputeSHA256(config)})
	if observed, found, readErr := pueue.ReadPrivateRecoveryBinding(fixture.store.Root); readErr != nil || !found || observed != binding {
		t.Fatalf("invalid historical recovery alias fixture: %v", readErr)
	}
}

func TestControlContinuationGroupCleanupRequiresExplicitRoute(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	writeGenerationGroup(t, fixture.store, fixture.legacy)
	refused := errors.New("fixture refuses continuation cleanup")
	calls := 0
	routing := selectedControlRouting{continuation: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		return infrastructure.Disposition{}, refused
	}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Routing: routing}).PrepareContinuation(t.Context(), fixture.store, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("pending cleanup skipped owner")))
	}
	if !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("pending cleanup used ordinary Saved recovery: calls=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("ordinary caller changed pending cleanup")
	}
}

func TestControlContinuationDifferentAliasPreservesCanonicalRefusal(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	writeContinuationAlias(t, fixture, fixture.legacy)
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options}).PrepareContinuation(t.Context(), fixture.store, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("unresolved generation returned ready client")))
	}
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unresolved generation lost canonical refusal: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("unresolved generation changed durable state")
	}
}

func TestControlContinuationSameActiveAliasRemainsPassive(t *testing.T) {
	fixture, _ := newWarmContinuationFixture(t)
	// Same-identity recovery data needs no retirement. Original metadata still
	// names A, so use B's immutable metadata to exercise this warm alias row.
	source := routedMetadataSourceForTask(t, fixture, fixture.target, strings.Repeat("b", 32))
	writeContinuationAlias(t, fixture, fixture.target)
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options}).PrepareContinuation(t.Context(), fixture.store, source)
	if client != nil {
		defer func() {
			if closeErr := client.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if err != nil || client == nil || client.Binding() != fixture.target {
		t.Fatalf("same-identity active alias required owner: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("same-identity alias entered cleanup")
	}
}

func TestControlContinuationReadsActualThirdGenerationAfterRoute(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	second := fixture.target
	third := parentMigrationPair(t, fixture, "third-generation")
	dataDirectory := filepath.Join(filepath.Dir(second.ConfigPath), "data")
	if err := os.Remove(dataDirectory); err != nil {
		t.Fatal(err)
	}
	calls := 0
	routing := selectedControlRouting{continuation: func(_ context.Context, actual *taskdir.Store, _ infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if actual != fixture.store || observed != source {
			t.Fatal("continuation route lost original record slot")
		}
		// Simulate the resident's completed durable result. No native Start or
		// generation API is bypassed by the production consumer under test.
		if err := os.Mkdir(dataDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		advanced := fixture
		advanced.target = third
		writeContinuationGeneration(t, advanced, []parentMigrationRecordFixture{
			{task.SchemaVersion, parentMigrationIdentity(fixture.legacy), parentMigrationIdentity(second), 12345, true},
			{task.SchemaVersion, parentMigrationIdentity(second), parentMigrationIdentity(third), 12346, true},
		})
		return infrastructure.Disposition{Kind: infrastructure.DispositionWarmReuse}, nil
	}}
	client, err := (ControlRuntime{Options: fixture.options, Routing: routing}).PrepareContinuation(t.Context(), fixture.store, source)
	if client != nil {
		defer func() {
			if closeErr := client.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if err != nil || client == nil || calls != 1 {
		t.Fatalf("third generation failed passive readback: calls=%d err=%v", calls, err)
	}
	if client.Binding() != third || client.RuntimeBinding() != third {
		t.Fatal("continuation returned stale predecessor or prepared candidate")
	}
	original, readErr := ReadTaskMetadataSupervisor(t.Context(), fixture.store, source)
	if readErr != nil || original != fixture.legacy {
		t.Fatal("continuation rewrote immutable predecessor metadata")
	}
}
