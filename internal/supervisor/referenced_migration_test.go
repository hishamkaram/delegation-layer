package supervisor

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type referencedMigrationTestGuard struct{}

func (referencedMigrationTestGuard) GuardDaemonStart(context.Context, string, func() error) error {
	return errors.New("fixture must not start a daemon")
}

type referencedMigrationTestWait struct{}

func (referencedMigrationTestWait) RegisterDaemonWait(string, int, <-chan struct{}) error {
	return errors.New("fixture must not register a daemon")
}

// This component fixture records canonical publication policy, not native
// resident placement. Start remains forbidden by referencedMigrationTestGuard.
type referencedMigrationCommitRecorder struct {
	root  string
	mu    sync.Mutex
	calls int
}

func (g *referencedMigrationCommitRecorder) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if root != g.root || commit == nil {
		return task.ErrIdentityMismatch
	}
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return commit()
}

func (g *referencedMigrationCommitRecorder) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

type parentMigrationFixture struct {
	store   *taskdir.Store
	options pueue.Options
	target  task.SupervisorRef
	legacy  task.SupervisorRef
	status  string
	commits *referencedMigrationCommitRecorder
}

const parentMigrationClient = `#!/bin/sh
set -eu
case "${3-}" in
  --version)
    if [ "${REFERENCED_FAIL_VERSION-}" = 1 ]; then exit 7; fi
    printf '%s\n' 'pueue 4.0.4'
    ;;
  status)
    if [ "${REFERENCED_UNAVAILABLE-}" = 1 ]; then exit 1; fi
    /bin/cat "$REFERENCED_STATUS"
    ;;
  *) exit 64 ;;
esac
`

func newParentMigrationFixture(t *testing.T) parentMigrationFixture {
	t.Helper()
	root := snapshotFixtureRoot(t)
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fixture := parentMigrationFixture{store: store, status: filepath.Join(root, "status.json"), commits: &referencedMigrationCommitRecorder{root: root}}
	writeParentMigrationConfig(t, root)
	if err = os.WriteFile(fixture.status, []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.options = pueue.Options{
		PrivateGenerationGuard:        fixture.commits,
		Environment:                   []string{"HOME=" + filepath.Join(root, "home-C"), "XDG_DATA_HOME=" + filepath.Join(root, "data-C"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config-C"), "PATH=/absent-selected-bin", "REFERENCED_STATUS=" + fixture.status},
		PrivateMaintenanceEnvironment: []string{},
		DaemonStartGuard:              referencedMigrationTestGuard{}, DaemonWaitOwner: referencedMigrationTestWait{}, WaitForPendingClose: true,
	}
	fixture.legacy = parentMigrationPair(t, fixture, "X")
	fixture.target = parentMigrationPair(t, fixture, "Y")
	// Mutable inventory Z deliberately disagrees with the retained Y record.
	parentMigrationPair(t, fixture, "Z")
	return fixture
}

func writeParentMigrationConfig(t *testing.T, root string) {
	t.Helper()
	base := filepath.Join(root, ".supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := task.MarshalCanonical(snapshotClientConfig{Shared: snapshotSharedConfig{
		PueueDirectory: base, RuntimeDirectory: base, UnixSocketPath: filepath.Join(base, "socket"),
		AliasFile: "~/.selected-aliases", PIDPath: filepath.Join(base, "pid"), SharedSecretPath: filepath.Join(base, "secret"),
		DaemonCert: filepath.Join(base, "cert"), DaemonKey: filepath.Join(base, "key"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pueue.PrivateConfigPath(root), config, 0o600); err != nil {
		t.Fatal(err)
	}
}

func parentMigrationPair(t *testing.T, fixture parentMigrationFixture, version string) task.SupervisorRef {
	t.Helper()
	client, daemon := statePairStagingSources(t, parentMigrationClient+"\n# client-"+version+"\n", "#!/bin/sh\nexit 0\n# daemon-"+version+"\n")
	client, daemon, err := InstallStateSupervisorPairPaths(t.Context(), fixture.store.Root, client, daemon)
	if err != nil {
		t.Fatal(err)
	}
	bound, bindErr := pueue.Bind(t.Context(), client, pueue.PrivateConfigPath(fixture.store.Root), fixture.options)
	if bindErr != nil {
		t.Fatal(errors.Join(bindErr, bound.CloseAfterPending(bindErr)))
	}
	binding := bound.Binding()
	if closeErr := bound.CloseAfterPending(nil); closeErr != nil {
		t.Fatal(closeErr)
	}
	pair := savedSupervisorPairRef(t, client, daemon)
	binding.DaemonExecutable, binding.DaemonSHA256 = daemon, pair.DaemonSHA256
	return binding
}

func parentMigrationIdentity(binding task.SupervisorRef) pueue.PrivateMigrationIdentity {
	return pueue.PrivateMigrationIdentity{
		Executable: binding.DaemonExecutable, SHA256: binding.DaemonSHA256,
		ConfigPath: binding.ConfigPath, ConfigDigest: binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256, Endpoint: binding.Endpoint,
	}
}

func parentMigrationResolver(t *testing.T, fixture parentMigrationFixture) retainedMigrationTargetResolver {
	t.Helper()
	pin, err := fixture.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	options, err := referencedMigrationOptions(fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	return retainedMigrationTargetResolver{store: fixture.store, pin: pin, options: options}
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
		if info.Mode().IsRegular() {
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

func TestRetainedMigrationTargetResolvesYFromSelectedContextReadOnly(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	t.Setenv("HOME", filepath.Join(fixture.store.Root, "ambient-home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(fixture.store.Root, "ambient-data"))
	fixture.options.Resolution = &pueue.ResolutionContext{OS: runtime.GOOS, Home: "/stale-X"}
	resolver := parentMigrationResolver(t, fixture)
	config, err := os.ReadFile(fixture.target.ConfigPath)
	if err != nil || len(config) == 0 {
		t.Fatalf("read-only Bind fixture needs an existing nonempty private config: %v", err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	got, err := resolver.ResolvePrivateMigrationTarget(t.Context(), fixture.store.Root, parentMigrationIdentity(fixture.target))
	if err != nil {
		t.Fatal(err)
	}
	if got != fixture.target || got.ClientExecutable == fixture.legacy.ClientExecutable || got.ResolutionHome != filepath.Join(fixture.store.Root, "home-C") {
		t.Fatal("resolver inherited X, ambient context or mutable inventory Z")
	}
	if runtime.GOOS == "linux" && got.ResolutionDataLocal != filepath.Join(fixture.store.Root, "data-C") {
		t.Fatal("resolver discarded selected XDG context")
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("read-only target resolution changed files, bytes or modes")
	}
}

func TestRetainedMigrationTargetMissingConfigRefusesWithoutWrites(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	if err := os.Remove(fixture.target.ConfigPath); err != nil {
		t.Fatal(err)
	}
	resolver := parentMigrationResolver(t, fixture)
	before := parentMigrationTree(t, fixture.store.Root)
	_, err := resolver.ResolvePrivateMigrationTarget(t.Context(), fixture.store.Root, parentMigrationIdentity(fixture.target))
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("missing private config was recreated or refused resolution wrote state")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing private config lost its read-only refusal: %v", err)
	}
}

func TestRetainedMigrationTargetRefusesChangedInputsReadOnly(t *testing.T) {
	for _, kind := range []string{"unsupported", "basename", "directory", "client", "daemon", "config", "logical", "context"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newParentMigrationFixture(t)
			identity := corruptParentMigrationFixture(t, &fixture, kind)
			resolver := parentMigrationResolver(t, fixture)
			before := parentMigrationTree(t, fixture.store.Root)
			_, resolveErr := resolver.ResolvePrivateMigrationTarget(t.Context(), fixture.store.Root, identity)
			if resolveErr == nil {
				t.Fatal("changed retained target or context accepted")
			}
			if (kind == "unsupported" || kind == "basename") && !errors.Is(resolveErr, pueue.ErrReferencedMigrationUnsupported) {
				t.Fatalf("historical target lost unsupported classification: %v", resolveErr)
			}
			if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
				t.Fatal("refused resolution changed durable state")
			}
		})
	}
}

func corruptParentMigrationFixture(t *testing.T, fixture *parentMigrationFixture, kind string) pueue.PrivateMigrationIdentity {
	t.Helper()
	identity := parentMigrationIdentity(fixture.target)
	switch kind {
	case "unsupported":
		identity.Executable = filepath.Join(fixture.store.Root, "external-pueued")
	case "basename":
		identity.Executable = filepath.Join(filepath.Dir(identity.Executable), "other-daemon")
	case "directory":
		destination := filepath.Join(filepath.Dir(filepath.Dir(identity.Executable)), "pair-"+strings.Repeat("d", 64))
		if err := os.Rename(filepath.Dir(identity.Executable), destination); err != nil {
			t.Fatal(err)
		}
		identity.Executable = filepath.Join(destination, "pueued")
	case "client", "daemon":
		path := fixture.target.ClientExecutable
		if kind == "daemon" {
			path = fixture.target.DaemonExecutable
		}
		if err := os.WriteFile(path, []byte("corrupt retained bytes"), 0o700); err != nil {
			t.Fatal(err)
		}
	case "config":
		if err := os.WriteFile(identity.ConfigPath, []byte("invalid config"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "logical":
		identity.ConfigDigest = strings.Repeat("e", 64)
	case "context":
		fixture.options.Environment = append(fixture.options.Environment[:0:0], fixture.options.Environment...)
		fixture.options.Environment[0] = "HOME=" + filepath.Join(fixture.store.Root, "wrong-home")
	}
	return identity
}

// These typed immutable bytes are historical fixtures, not an alternate
// migration writer or policy. Production lower codecs validate every read.
type parentMigrationRecordFixture struct {
	SchemaVersion int                            `json:"schema_version"`
	Legacy        pueue.PrivateMigrationIdentity `json:"legacy"`
	Successor     pueue.PrivateMigrationIdentity `json:"successor"`
	LegacyPID     int                            `json:"legacy_pid"`
	LegacyStopped bool                           `json:"legacy_stopped"`
}

func publishParentMigrationHistory(t *testing.T, fixture parentMigrationFixture, pending bool) infrastructure.MigrationReference {
	t.Helper()
	record := parentMigrationRecordFixture{task.SchemaVersion, parentMigrationIdentity(fixture.legacy), parentMigrationIdentity(fixture.target), 12345, true}
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int                            `json:"schema_version"`
		Migrations    []parentMigrationRecordFixture `json:"migrations"`
	}{task.SchemaVersion, []parentMigrationRecordFixture{record}})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(fixture.target.ConfigPath)
	if err = os.WriteFile(filepath.Join(base, "migration-history.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if pending {
		data, err = task.MarshalCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(base, "migration.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	projection, err := task.MarshalCanonical(struct {
		SchemaVersion int                            `json:"schema_version"`
		Legacy        pueue.PrivateMigrationIdentity `json:"legacy"`
		Successor     pueue.PrivateMigrationIdentity `json:"successor"`
	}{task.SchemaVersion, record.Legacy, record.Successor})
	if err != nil {
		t.Fatal(err)
	}
	writeHistoricalSupervisorBinding(t, fixture.store.Root, fixture.target)
	return infrastructure.MigrationReference{RootID: fixture.store.RootID, RecordSHA256: task.ComputeSHA256(projection), HistoryIndex: 0}
}

func TestReferencedMigrationParentCompletesRecordedYAndWarmReuses(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	ref := publishParentMigrationHistory(t, fixture, true)
	for index, want := range []pueue.PrivateMigrationResult{pueue.PrivateMigrationCompleted, pueue.PrivateMigrationWarmReuse} {
		got, err := ExecuteReferencedPrivateMigration(t.Context(), fixture.store, ref, fixture.options)
		if err != nil || got != want {
			t.Fatalf("history consumption %d: result=%s error=%v", index, got, err)
		}
	}
	if fixture.commits.count() == 0 {
		t.Fatal("canonical migration completion bypassed its publication owner")
	}
	if _, err := fixture.store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("consumer closed the borrowed store: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(fixture.target.ConfigPath), "migration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical completion did not clear pending journal: %v", err)
	}
}

func TestReferencedMigrationParentRefusesMissingPublicationOwner(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	ref := publishParentMigrationHistory(t, fixture, true)
	fixture.options.PrivateGenerationGuard = nil
	// Initialize the canonical election lock before checking that refusal
	// preserves the entire existing tree, including migration history.
	acquired, err := pueue.TryWithPrivateTransition(t.Context(), fixture.store.Root, func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("initialize fixture election: acquired=%t error=%v", acquired, err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	got, err := ExecuteReferencedPrivateMigration(t.Context(), fixture.store, ref, fixture.options)
	if !errors.Is(err, pueue.ErrInfrastructureOwnerRequired) || got != "" || fixture.commits.count() != 0 {
		t.Fatalf("missing publication owner accepted: result=%s calls=%d error=%v", got, fixture.commits.count(), err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("missing publication owner changed migration history")
	}
}

func TestReferencedMigrationParentCurrentYNotReady(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	ref := publishParentMigrationHistory(t, fixture, false)
	fixture.options.Environment = append(fixture.options.Environment, "REFERENCED_UNAVAILABLE=1")
	if _, err := ExecuteReferencedPrivateMigration(t.Context(), fixture.store, ref, fixture.options); !errors.Is(err, pueue.ErrReferencedMigrationTargetNotReady) {
		t.Fatalf("current unavailable Y lost distinct cause: %v", err)
	}
}

func TestReferencedMigrationParentRejectsReferencesAndCancellation(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	ref := publishParentMigrationHistory(t, fixture, false)
	for _, kind := range []string{"root", "digest", "overflow", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			changed, ctx := ref, t.Context()
			switch kind {
			case "root":
				changed.RootID = strings.Repeat("e", 32)
			case "digest":
				changed.RecordSHA256 = ""
			case "overflow":
				changed.HistoryIndex = uint64(math.MaxInt) + 1
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			before := parentMigrationTree(t, fixture.store.Root)
			_, err := ExecuteReferencedPrivateMigration(ctx, fixture.store, changed, fixture.options)
			if err == nil || (kind == "canceled" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("invalid reference or cancellation accepted: %v", err)
			}
			if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
				t.Fatal("invalid reference modified storage")
			}
		})
	}
}

func TestRetainedMigrationTargetPreservesFiniteBindFailure(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	fixture.options.Environment = append(fixture.options.Environment, "REFERENCED_FAIL_VERSION=1")
	resolver := parentMigrationResolver(t, fixture)
	before := parentMigrationTree(t, fixture.store.Root)
	_, err := resolver.ResolvePrivateMigrationTarget(t.Context(), fixture.store.Root, parentMigrationIdentity(fixture.target))
	var exitError *exec.ExitError
	if !errors.Is(err, pueue.ErrBinding) || !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("finite Bind failure or its underlying cause was lost: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("failed Bind changed storage")
	}
}

func TestRetainedMigrationTargetRevalidatesAfterNativeBind(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	var once sync.Once
	modified := make(chan error, 1)
	fixture.options.Observer = func(event pueue.CommandEvent) {
		if event.Stage == "completed" {
			once.Do(func() {
				modified <- os.WriteFile(fixture.target.ClientExecutable, []byte("changed after version read"), 0o700)
			})
		}
	}
	resolver := parentMigrationResolver(t, fixture)
	if _, err := resolver.ResolvePrivateMigrationTarget(t.Context(), fixture.store.Root, parentMigrationIdentity(fixture.target)); err == nil {
		t.Fatal("pair changed during native Bind was accepted")
	}
	select {
	case err := <-modified:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("native Bind did not reach the completion mutation seam")
	}
}

func TestReferencedMigrationParentReportsElectionContention(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	ref := publishParentMigrationHistory(t, fixture, false)
	acquired, err := pueue.TryWithPrivateTransition(t.Context(), fixture.store.Root, func() error {
		_, executeErr := ExecuteReferencedPrivateMigration(t.Context(), fixture.store, ref, fixture.options)
		if !errors.Is(executeErr, task.ErrLockBusy) || errors.Is(executeErr, pueue.ErrPrivateSupervisorMigrationBusy) {
			t.Fatalf("election contention asserted migration work: %v", executeErr)
		}
		return nil
	})
	if err != nil || !acquired {
		t.Fatalf("fixture did not own transition: acquired=%t err=%v", acquired, err)
	}
}

func replaceParentMigrationRootAfterBind(t *testing.T, fixture *parentMigrationFixture) (string, <-chan error) {
	t.Helper()
	root, retained := fixture.store.Root, fixture.store.Root+"-retained"
	if _, err := os.Lstat(retained); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture replacement destination is not absent: %v", err)
	}
	// Restore the sole owned root before the fixture closes Store and removes
	// its directory. Mutation happens after the finite version process exits.
	t.Cleanup(func() {
		if _, err := os.Lstat(retained); errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			t.Errorf("retained fixture root %s: %v", retained, err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("retained fixture root %s: %v", retained, err)
			return
		}
		if err := os.Rename(retained, root); err != nil {
			t.Errorf("retained fixture root %s: %v", retained, err)
		}
	})
	var once sync.Once
	modified := make(chan error, 1)
	fixture.options.Observer = func(event pueue.CommandEvent) {
		if event.Stage == "completed" {
			once.Do(func() {
				if err := os.Rename(root, retained); err != nil {
					modified <- err
					return
				}
				modified <- os.Mkdir(root, 0o700)
			})
		}
	}
	return retained, modified
}

func TestRetainedMigrationTargetRefusesRootReplacementAfterBind(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	root := fixture.store.Root
	retained, modified := replaceParentMigrationRootAfterBind(t, &fixture)
	resolver := parentMigrationResolver(t, fixture)
	before := parentMigrationTree(t, root)
	_, err := resolver.ResolvePrivateMigrationTarget(t.Context(), root, parentMigrationIdentity(fixture.target))
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("native root replacement lost identity fault: %v", err)
	}
	select {
	case mutationErr := <-modified:
		if mutationErr != nil {
			t.Fatal(mutationErr)
		}
	default:
		t.Fatal("Bind did not reach root replacement seam")
	}
	if !maps.Equal(before, parentMigrationTree(t, retained)) {
		t.Fatal("read-only resolution changed the retained original root")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("resolver wrote into replacement root: %v", err)
	}
}

func TestReferencedMigrationOptionsRequireExplicitSelectedEnvironment(t *testing.T) {
	options := pueue.Options{Environment: []string{}, PrivateMaintenanceEnvironment: []string{}, DaemonStartGuard: referencedMigrationTestGuard{}, DaemonWaitOwner: referencedMigrationTestWait{}, Resolution: &pueue.ResolutionContext{Home: "/stale-X"}}
	selected, err := referencedMigrationOptions(options)
	if err != nil || selected.Environment == nil || len(selected.Environment) != 0 || selected.Resolution != nil || !selected.WaitForPendingClose {
		t.Fatalf("explicit empty snapshot changed or inherited X resolution: %v", err)
	}
	options.Environment = nil
	if _, err = referencedMigrationOptions(options); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil selected environment accepted ambient fallback: %v", err)
	}
}

func TestReferencedMigrationOptionsOwnSelectedSnapshotAndPorts(t *testing.T) {
	options := pueue.Options{Environment: []string{"HOME=/selected-C"}, PrivateMaintenanceEnvironment: []string{"MAINTENANCE=selected-C"}, DaemonStartGuard: referencedMigrationTestGuard{}, DaemonWaitOwner: referencedMigrationTestWait{}}
	selected, err := referencedMigrationOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	options.Environment[0] = "HOME=/changed-after-capture"
	options.PrivateMaintenanceEnvironment[0] = "MAINTENANCE=changed-after-capture"
	if selected.Environment[0] != "HOME=/selected-C" || selected.PrivateMaintenanceEnvironment[0] != "MAINTENANCE=selected-C" || selected.DaemonStartGuard != options.DaemonStartGuard || selected.DaemonWaitOwner != options.DaemonWaitOwner {
		t.Fatal("selected snapshot aliases its caller or lost native owner ports")
	}
	for _, kind := range []string{"guard", "wait", "malformed-environment"} {
		t.Run(kind, func(t *testing.T) {
			changed := options
			want := os.ErrInvalid
			switch kind {
			case "guard":
				changed.DaemonStartGuard = nil
			case "wait":
				changed.DaemonWaitOwner = nil
			case "malformed-environment":
				changed.Environment = []string{"invalid"}
				want = pueue.ErrConfiguration
			}
			if _, optionsErr := referencedMigrationOptions(changed); !errors.Is(optionsErr, want) {
				t.Fatalf("invalid accepted options were not refused: %v", optionsErr)
			}
		})
	}
}

func TestReferencedMigrationParentRejectsUnselectedMaintenanceBeforeWrites(t *testing.T) {
	fixture := newParentMigrationFixture(t)
	ref := publishParentMigrationHistory(t, fixture, false)
	for _, test := range []struct {
		name        string
		environment []string
		want        error
	}{
		{"nil", nil, os.ErrInvalid},
		{"malformed", []string{"invalid"}, pueue.ErrConfiguration},
		{"duplicate", []string{"SELECTED=first", "SELECTED=second"}, pueue.ErrConfiguration},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := fixture.options
			options.PrivateMaintenanceEnvironment = test.environment
			before := parentMigrationTree(t, fixture.store.Root)
			_, err := ExecuteReferencedPrivateMigration(t.Context(), fixture.store, ref, options)
			if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
				t.Fatal("unselected maintenance snapshot reached migration mutation")
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("invalid maintenance snapshot was accepted: %v", err)
			}
		})
	}
}
