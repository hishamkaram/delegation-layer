package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type selectedControlRouting struct {
	controlRoutingFunc
	saved        func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error)
	continuation func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error)
	manual       func(context.Context, *taskdir.Store, infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error)
}

type savedRoutingFixtureConfig struct {
	Shared savedRoutingFixtureShared `json:"shared"`
	Daemon savedRoutingFixtureDaemon `json:"daemon"`
}

type savedRoutingFixtureShared struct {
	snapshotSharedConfig
	Permissions uint32 `json:"unix_socket_permissions"`
}

type savedRoutingFixtureDaemon struct {
	ShellCommand []string `json:"shell_command"`
}

func newSavedControlFixture(t *testing.T) parentMigrationFixture {
	t.Helper()
	store, err := taskdir.InitStore(snapshotFixtureRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	base := filepath.Dir(pueue.PrivateConfigPath(store.Root))
	if err = os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := task.MarshalCanonical(savedRoutingFixtureConfig{Shared: savedRoutingFixtureShared{snapshotSharedConfig: snapshotSharedConfig{
		PueueDirectory: filepath.Join(base, "data"), RuntimeDirectory: filepath.Join(base, "run"), UnixSocketPath: filepath.Join(base, "socket"),
		AliasFile: filepath.Join(base, "aliases.yml"), PIDPath: filepath.Join(base, "run", "pueue.pid"), SharedSecretPath: filepath.Join(base, "data", "shared_secret"),
		DaemonCert: filepath.Join(base, "data", "certs", "daemon.cert"), DaemonKey: filepath.Join(base, "data", "certs", "daemon.key"),
	}, Permissions: 0o700}, Daemon: savedRoutingFixtureDaemon{ShellCommand: []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pueue.PrivateConfigPath(store.Root), config, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := parentMigrationFixture{store: store, status: filepath.Join(store.Root, "status.json")}
	if err = os.WriteFile(fixture.status, []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.options = pueue.Options{Environment: []string{"HOME=" + store.Root, "CONTROL_FIXTURE_STATUS=" + fixture.status}, PrivateMaintenanceEnvironment: []string{}, DaemonStartGuard: controlFixtureNoStart{}, DaemonWaitOwner: controlFixtureNoWait{}, WaitForPendingClose: true}
	fixture.target = parentMigrationPair(t, fixture, "routing")
	return fixture
}

func (r selectedControlRouting) RecoverSaved(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	if r.saved == nil {
		return infrastructure.Disposition{}, errors.New("unexpected saved routing")
	}
	return r.saved(ctx, store, selected, source)
}

func (r selectedControlRouting) PrepareContinuation(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	if r.continuation == nil {
		return infrastructure.Disposition{}, errors.New("unexpected continuation routing")
	}
	return r.continuation(ctx, store, selected, source)
}

func (r selectedControlRouting) MigrateOwn(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	if r.manual == nil {
		return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, errors.New("unexpected manual routing")
	}
	return r.manual(ctx, store, selected)
}

// These fixtures exercise public control decisions and durable target effects.
// Their finite command scripts confer no native resident or daemon authority.
func TestControlRuntimeSavedReplyRequiresFreshReadyProof(t *testing.T) {
	for _, contactError := range []bool{false, true} {
		t.Run(map[bool]string{false: "unready reply", true: "unknown contact"}[contactError], func(t *testing.T) {
			exerciseSavedControlReply(t, contactError)
		})
	}
}

func exerciseSavedControlReply(t *testing.T, contactError bool) {
	t.Helper()
	fixture := newSavedControlFixture(t)
	source, err := NewSavedBindingSource(fixture.store.RootID, infrastructure.SavedBindingTaskSubmission, strings.Repeat("a", 32), "", fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	unknown := errors.New("unknown routed outcome")
	calls := 0
	routing := selectedControlRouting{saved: func(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if store != fixture.store || observed != source || selected.Root != fixture.store.Root || selected.RootID != fixture.store.RootID {
			t.Fatal("saved routing changed canonical target or subject")
		}
		if _, finite := ctx.Deadline(); !finite {
			t.Fatal("saved routing lost finite control context")
		}
		if contactError {
			return infrastructure.Disposition{}, unknown
		}
		return infrastructure.Disposition{Kind: infrastructure.DispositionWarmReuse}, nil
	}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: fixture.target.ClientExecutable}, Routing: routing}).RecoverSaved(t.Context(), fixture.store, fixture.target, source)
	if client != nil {
		t.Fatal(errors.Join(errors.New("unready reply produced a client"), client.CloseAfterPending(err)))
	}
	want := ErrInfrastructureAuthorityUnavailable
	if contactError {
		want = unknown
	}
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("saved reply replaced proof or retried contact: calls=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("saved cold observation/contact entered local recovery")
	}
}

func TestControlRuntimeSavedTaskObservationReadyAliasAvoidsOwner(t *testing.T) {
	fixture, source, runtimeBinding := newRoutedTaskObservationAliasFixture(t)
	before := parentMigrationTree(t, fixture.store.Root)
	calls := 0
	routing := selectedControlRouting{saved: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		return infrastructure.Disposition{}, errors.New("ready task alias must not contact owner")
	}}
	client, err := (ControlRuntime{Options: fixture.options, Routing: routing}).RecoverSavedTaskObservation(t.Context(), fixture.store, fixture.target, source)
	requireCompatibleControlClient(t, client, err, fixture.target, runtimeBinding)
	if calls != 0 {
		t.Fatalf("ready task alias contacted owner %d times", calls)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("ready task observation changed durable alias or migration records")
	}
	assertGeneralSavedRecoveryRefusesTaskAlias(t, fixture, source)
}

func TestControlRuntimeSavedTaskObservationReprobesAliasAfterOwner(t *testing.T) {
	fixture, source, runtimeBinding := newRoutedTaskObservationAliasFixture(t)
	dataDirectory := filepath.Join(filepath.Dir(fixture.target.ConfigPath), "data")
	if err := os.Remove(dataDirectory); err != nil {
		t.Fatal(err)
	}
	pin, err := fixture.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var afterOwner map[string]parentMigrationTreeEntry
	routing := selectedControlRouting{saved: func(ctx context.Context, actual *taskdir.Store, selected infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if actual != fixture.store || observed != source || selected.Root != pin.Root() || selected.RootID != pin.RootID() {
			t.Fatal("task alias recovery changed its borrowed store or saved subject")
		}
		borrowed, pinErr := actual.PinRootContext(ctx)
		if pinErr != nil || !borrowed.Equal(pin) {
			t.Fatalf("owner contact lost the caller's live root pin: %v", pinErr)
		}
		saved, readErr := ReadTaskMetadataSupervisor(ctx, actual, observed)
		if readErr != nil || saved != fixture.target {
			t.Fatalf("owner contact could not read the original task binding: %v", readErr)
		}
		if mkdirErr := os.Mkdir(dataDirectory, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		afterOwner = parentMigrationTree(t, fixture.store.Root)
		return infrastructure.Disposition{Kind: infrastructure.DispositionWarmReuse}, nil
	}}
	client, err := (ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: fixture.target.ClientExecutable}, Routing: routing}).RecoverSavedTaskObservation(t.Context(), fixture.store, fixture.target, source)
	requireCompatibleControlClient(t, client, err, fixture.target, runtimeBinding)
	if calls != 1 {
		t.Fatalf("task alias recovery contacted owner %d times, want one", calls)
	}
	if !maps.Equal(afterOwner, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("second task probe changed the owner's recovered state")
	}
	assertGeneralSavedRecoveryRefusesTaskAlias(t, fixture, source)
}

// Reuse the canonical store and finite command fixture. These prepared records
// model an owner reply; the no-start/no-wait guards confer no daemon authority.
func newRoutedTaskObservationAliasFixture(t *testing.T) (parentMigrationFixture, infrastructure.SavedBindingSource, task.SupervisorRef) {
	t.Helper()
	fixture, _ := newWarmContinuationFixture(t)
	source := routedMetadataSourceForTask(t, fixture, fixture.target, strings.Repeat("b", 32))
	config, err := os.ReadFile(fixture.target.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	savedConfig, err := pueue.ParseConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	var document savedRoutingFixtureConfig
	if err = json.Unmarshal(config, &document); err != nil {
		t.Fatal(err)
	}
	// Re-encode the authored fixture schema with whitespace only; its effective
	// config stays identical while the alias's own raw digest changes.
	alias, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	alias = append(alias, '\n')
	aliasConfig, err := pueue.ParseConfig(alias)
	if err != nil || !reflect.DeepEqual(savedConfig, aliasConfig) {
		t.Fatalf("fixture reserialization changed the parsed native config: %v", err)
	}
	base := filepath.Dir(fixture.target.ConfigPath)
	aliasPath := filepath.Join(base, "legacy-pueue.yml")
	if err = os.WriteFile(aliasPath, alias, 0o600); err != nil {
		t.Fatal(err)
	}
	aliasDigest := task.ComputeSHA256(alias)
	if aliasDigest == fixture.target.ConfigDigest {
		t.Fatal("equivalent alias did not change the original raw config")
	}
	writeContinuationFixtureRecord(t, filepath.Join(base, "legacy-supervisor.json"), struct {
		SchemaVersion int                `json:"schema_version"`
		Supervisor    task.SupervisorRef `json:"supervisor"`
		ConfigPath    string             `json:"config_path"`
		ConfigSHA256  string             `json:"config_sha256"`
	}{task.SchemaVersion, fixture.target, aliasPath, aliasDigest})
	document.Shared.UnixSocketPath = filepath.Join(base, "next.sock")
	changed, err := task.MarshalCanonical(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fixture.target.ConfigPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	successor := parentMigrationPair(t, fixture, "task-observation-successor")
	writeContinuationFixtureRecord(t, filepath.Join(base, "migration.json"), parentMigrationRecordFixture{
		SchemaVersion: task.SchemaVersion, Legacy: parentMigrationIdentity(fixture.target), Successor: parentMigrationIdentity(successor),
	})
	if pending, pendingErr := pueue.PrivateSupervisorMigrationPending(fixture.store.Root); pendingErr != nil || !pending {
		t.Fatalf("task alias fixture lacks a valid prepared migration: pending=%t err=%v", pending, pendingErr)
	}
	runtimeBinding := fixture.target
	runtimeBinding.ConfigPath, runtimeBinding.ConfigDigest = aliasPath, aliasDigest
	return fixture, source, runtimeBinding
}

func assertGeneralSavedRecoveryRefusesTaskAlias(t *testing.T, fixture parentMigrationFixture, source infrastructure.SavedBindingSource) {
	t.Helper()
	for _, open := range []bool{false, true} {
		refused := errors.New("general recovery requires owner preparation")
		calls := 0
		runtime := ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: fixture.target.ClientExecutable}, Routing: selectedControlRouting{saved: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
			calls++
			return infrastructure.Disposition{}, refused
		}}, OpenStore: func(ctx context.Context, root string) (*taskdir.Store, error) {
			return taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
		}}
		before := parentMigrationTree(t, fixture.store.Root)
		var client *pueue.Client
		var err error
		if open {
			client, err = runtime.OpenSaved(t.Context(), fixture.store.Root, fixture.target, source)
		} else {
			client, err = runtime.RecoverSaved(t.Context(), fixture.store, fixture.target, source)
		}
		if client != nil {
			t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("general recovery accepted a task-only alias")))
		}
		if !errors.Is(err, refused) || calls != 1 {
			t.Fatalf("general recovery broadened task alias readiness: open=%t calls=%d err=%v", open, calls, err)
		}
		after := parentMigrationTree(t, fixture.store.Root)
		if open && before[".probe"].mode.IsDir() && after[".probe"].mode.IsDir() {
			// Opening Store probes filesystem support by creating and removing a
			// child of .probe. Only that existing directory's mtime may change.
			probe := after[".probe"]
			probe.modified = before[".probe"].modified
			after[".probe"] = probe
		}
		if !maps.Equal(before, after) {
			logSavedControlTreeChanges(t, before, after)
			t.Fatalf("general recovery mutated a prepared task alias: open=%t", open)
		}
	}
}

func logSavedControlTreeChanges(t *testing.T, before, after map[string]parentMigrationTreeEntry) {
	t.Helper()
	paths := make(map[string]struct{}, len(before)+len(after))
	for path := range before {
		paths[path] = struct{}{}
	}
	for path := range after {
		paths[path] = struct{}{}
	}
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		previous, wasPresent := before[path]
		current, isPresent := after[path]
		if wasPresent == isPresent && previous == current {
			continue
		}
		t.Logf("path=%q present=%t→%t mode=%v→%v modified=%s→%s content_changed=%t", path,
			wasPresent, isPresent, previous.mode, current.mode, previous.modified.Format(time.RFC3339Nano),
			current.modified.Format(time.RFC3339Nano), previous.content != current.content)
	}
}

func TestControlRuntimeManualPreservesReportAndSelectedContext(t *testing.T) {
	store := newGenerationStore(t)
	report := infrastructure.ManualMigrationReport{State: infrastructure.ManualMigrationNotInitialized, Legacy: infrastructure.ManualMigrationLegacyNotPresent, Active: infrastructure.ManualMigrationActiveNone}
	calls := 0
	routing := selectedControlRouting{manual: func(ctx context.Context, actual *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
		calls++
		if actual != store || selected.Root != store.Root || selected.RootID != store.RootID {
			t.Fatal("manual routing replaced actual target")
		}
		origin, found, err := infrastructure.ReadOrigin(selected.ControlEnvironment)
		if err != nil || !found || origin.RootID() != store.RootID {
			t.Fatal("manual routing lost selected target overlay")
		}
		if _, finite := ctx.Deadline(); !finite {
			t.Fatal("manual routing lost finite deadline")
		}
		return infrastructure.Disposition{}, report, nil
	}}
	before := observationTree(t, store.Root)
	disposition, observed, err := (ControlRuntime{Lookup: controlRoutingLookup(t), Routing: routing}).MigrateOwn(t.Context(), store)
	if err != nil || disposition != (infrastructure.Disposition{}) || observed != report || calls != 1 {
		t.Fatalf("valid not-initialized race lost report: calls=%d err=%v", calls, err)
	}
	if !slices.Equal(before, observationTree(t, store.Root)) {
		t.Fatal("manual routing performed local initialization")
	}
}

type routedStoreRetentionError struct {
	store *taskdir.Store
	cause error
}

func (e *routedStoreRetentionError) Error() string                 { return e.cause.Error() }
func (e *routedStoreRetentionError) Unwrap() error                 { return e.cause }
func (e *routedStoreRetentionError) RetainedStore() *taskdir.Store { return e.store }

func TestControlRuntimeOpenSavedRetainsCleanupStore(t *testing.T) {
	fixture := newSavedControlFixture(t)
	source, err := NewSavedBindingSource(fixture.store.RootID, infrastructure.SavedBindingTaskMetadata, strings.Repeat("a", 32), "", fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	retained := &routedStoreRetentionError{store: fixture.store, cause: errors.New("owned startup cleanup unresolved")}
	runtime := ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: fixture.target.ClientExecutable}, OpenStore: func(context.Context, string) (*taskdir.Store, error) { return fixture.store, nil }, Routing: selectedControlRouting{saved: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		return infrastructure.Disposition{}, retained
	}}}
	client, err := runtime.OpenSaved(t.Context(), fixture.store.Root, fixture.target, source)
	if client != nil || !errors.Is(err, retained) || !taskdir.CleanupRetainsStore(err, fixture.store) {
		t.Fatalf("routed cleanup owner lost: %v", err)
	}
	if _, pinErr := fixture.store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("routed cleanup prematurely closed borrowed store: %v", pinErr)
	}
	retained.store = nil
	if taskdir.CleanupRetainsStore(err, fixture.store) {
		t.Fatal("settled cleanup still retains store")
	}
}

func TestSubmittedQueueRepairRoutesCommittedSubmission(t *testing.T) {
	fixture := newSavedControlFixture(t)
	request := supervisorfixture.NewInspectionRequest(t, fixture.store)
	digest := task.ComputeSHA256([]byte("routing fixture"))
	meta := task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID,
		RequestedConfig: request.RequestedConfig, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment: "fixture-only", Approval: "never", ProviderExecutable: fixture.target.ClientExecutable, ProviderVersion: "fixture-v2",
		PublisherBuild: "routing-fixture", PublisherVersion: "routing-fixture", Predicate: task.FixturePredicateRef(),
		SupervisorConfig: fixture.target, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := fixture.store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submission := commitRoutedTaskSubmission(t, td, fixture.target)
	refused := errors.New("fixture refuses after submitted-source selection")
	calls := 0
	routing := selectedControlRouting{saved: func(_ context.Context, store *taskdir.Store, _ infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if store != fixture.store || source.Kind != infrastructure.SavedBindingTaskSubmission || source.OwnerID != submission.TaskID || source.RootID != submission.RootID {
			t.Fatal("submitted queue repair selected a preparation slot")
		}
		if sourceErr := source.ValidateBinding(submission.Supervisor); sourceErr != nil {
			t.Fatal(sourceErr)
		}
		return infrastructure.Disposition{}, refused
	}}
	err = RepairQueueForSubmittedTask(t.Context(), fixture.store, submission, &meta, AdmissionOptions{Control: fixture.options, InitialExecutable: fixture.target.ClientExecutable, Routing: routing})
	if !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("submitted queue repair bypassed exact recovery source: calls=%d err=%v", calls, err)
	}
}

func commitRoutedTaskSubmission(t *testing.T, td *taskdir.TaskDir, binding task.SupervisorRef) *task.SubmitRecord {
	t.Helper()
	permit, err := td.PrepareSubmission(binding)
	if permit != nil {
		t.Cleanup(func() {
			if closeErr := permit.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
		t.Fatal(err)
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	return submission
}

type controlFixtureNoStart struct{}

func (controlFixtureNoStart) GuardDaemonStart(context.Context, string, func() error) error {
	return errors.New("fixture must not start a daemon")
}

type controlFixtureNoWait struct{}

func (controlFixtureNoWait) RegisterDaemonWait(string, int, <-chan struct{}) error {
	return errors.New("fixture must not register a daemon")
}

type parentMigrationFixture struct {
	store   *taskdir.Store
	options pueue.Options
	target  task.SupervisorRef
	legacy  task.SupervisorRef
	status  string
}

const parentMigrationClient = `#!/bin/sh
set -eu
case "${3-}" in
  --version)
    printf '%s\n' 'pueue 4.0.4'
    ;;
  status)
    if [ "${CONTROL_FIXTURE_UNAVAILABLE-}" = 1 ]; then exit 1; fi
    /bin/cat "$CONTROL_FIXTURE_STATUS"
    ;;
  *) exit 64 ;;
esac
`

func parentMigrationPair(t *testing.T, fixture parentMigrationFixture, version string) task.SupervisorRef {
	t.Helper()
	client, daemon := installedPairFixture(t, parentMigrationClient+"\n# client-"+version+"\n", "#!/bin/sh\nexit 0\n# daemon-"+version+"\n")
	bound, bindErr := pueue.Bind(t.Context(), client, pueue.PrivateConfigPath(fixture.store.Root), fixture.options)
	if bindErr != nil {
		t.Fatal(errors.Join(bindErr, bound.CloseAfterPending(bindErr)))
	}
	binding := bound.Binding()
	if closeErr := bound.CloseAfterPending(nil); closeErr != nil {
		t.Fatal(closeErr)
	}
	binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	binding.ClientSHA256, binding.DaemonSHA256 = "", ""
	binding.DaemonExecutable = daemon
	return binding
}

type parentMigrationIdentityFixture struct {
	Executable           string                  `json:"executable"`
	SHA256               string                  `json:"sha256,omitempty"`
	ExecutableIdentity   task.ExecutableIdentity `json:"executable_identity,omitempty"`
	ConfigPath           string                  `json:"config_path,omitempty"`
	ConfigDigest         string                  `json:"config_digest,omitempty"`
	ResolvedConfigSHA256 string                  `json:"resolved_config_sha256,omitempty"`
	Endpoint             string                  `json:"endpoint,omitempty"`
}

func parentMigrationIdentity(binding task.SupervisorRef) parentMigrationIdentityFixture {
	return parentMigrationIdentityFixture{
		Executable: binding.DaemonExecutable, SHA256: binding.DaemonSHA256, ExecutableIdentity: binding.ExecutableIdentity,
		ConfigPath: binding.ConfigPath, ConfigDigest: binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256, Endpoint: binding.Endpoint,
	}
}

type parentMigrationTreeEntry struct {
	mode     fs.FileMode
	modified time.Time
	content  string
}

func parentMigrationTree(t *testing.T, root string) map[string]parentMigrationTreeEntry {
	t.Helper()
	entries := make(map[string]parentMigrationTreeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := parentMigrationTreeEntry{mode: info.Mode(), modified: info.ModTime()}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 == 0 {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			value.content = string(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
			value.content, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		entries[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

type parentMigrationRecordFixture struct {
	SchemaVersion int                            `json:"schema_version"`
	Legacy        parentMigrationIdentityFixture `json:"legacy"`
	Successor     parentMigrationIdentityFixture `json:"successor"`
	LegacyPID     int                            `json:"legacy_pid"`
	LegacyStopped bool                           `json:"legacy_stopped"`
}
