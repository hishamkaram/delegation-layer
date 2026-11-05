package supervisor

import (
	"context"
	"errors"
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

func TestInfrastructureOwnerArtifactsRejectsUnselectedRunningImage(t *testing.T) {
	for _, useCurrentImage := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-path", true: "unmanaged-test-image"}[useCurrentImage], func(t *testing.T) {
			target := newGenerationStore(t)
			candidate := filepath.Join(target.Root, "not-the-running-image")
			if useCurrentImage {
				var err error
				candidate, err = os.Executable()
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := StageInfrastructureOwnerArtifacts(t.Context(), target.Root, infrastructure.Artifacts{Runner: candidate}, ownerArtifactShapeRequirements(), target)
			if !errors.Is(err, infrastructure.ErrIdentity) {
				t.Fatalf("unselected or unmanaged running image accepted: %v", err)
			}
			requireNoInfrastructureRuntimeArtifacts(t, target.Root)
		})
	}
}

func TestInfrastructureArtifactCopiesKeepPublishedSourceAndTargetPin(t *testing.T) {
	sourceRoot, binding := infrastructureSourceFixture(t)
	candidate := buildInfrastructureRuntime(t)
	expectedRuntime, err := stateExecutableSourceDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := prepareInfrastructureRuntimeSource(t.Context(), sourceRoot, candidate)
	if err != nil {
		t.Fatal(err)
	}
	// Inventory is a staging view. The current published binding selects what
	// another root may copy, even when a different pair has been staged here.
	otherClient, otherDaemon := statePairStagingSources(t, "later-client", "later-daemon")
	if err = installStateSupervisorPair(t.Context(), sourceRoot, otherClient, otherDaemon); err != nil {
		t.Fatal(err)
	}
	pair, err := readInfrastructurePairSource(t.Context(), sourceRoot)
	if err != nil || pair.saved != binding {
		t.Fatalf("did not capture the published source: %v", err)
	}
	// Replace only this synthetic fixture publication after capture. Copies
	// must retain X even though a fresh reader would now observe Y.
	replaceOwnerArtifactPair(t, sourceRoot, binding, "changed-client", "changed-daemon")
	target := newGenerationStore(t)
	pin, err := target.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var staged infrastructure.Artifacts
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Taking EX here proves the source read released SH, and that copying does
	// not reacquire it while touching the target.
	err = pueue.WithBootstrapLockContext(ctx, sourceRoot, func(locked context.Context) error {
		var copyErr error
		staged, copyErr = stageInfrastructureArtifactCopies(locked, runtime, pair, ownerArtifactRequirements(expectedRuntime, binding), target, pin)
		return copyErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if staged.RunnerSHA256 != runtime.sha256 || staged.ClientSHA256 != binding.ClientSHA256 || staged.DaemonSHA256 != binding.DaemonSHA256 {
		t.Fatal("combined copy changed its frozen source identities")
	}
	requireStatePairSelectionUnchanged(t, filepath.Join(target.Root, ".supervisor"), nil, false)
	if _, found, readErr := pueue.ReadPrivateSupervisorBinding(target.Root); readErr != nil || found {
		t.Fatalf("content copy selected a target binding: found=%t err=%v", found, readErr)
	}
	// No process uses this fixture source. Target content must survive removal
	// after the source's exclusive lease has been released.
	if err = os.RemoveAll(filepath.Join(sourceRoot, ".supervisor")); err != nil {
		t.Fatal(err)
	}
	if err = ValidateInfrastructureArtifacts(t.Context(), target.Root, staged); err != nil {
		t.Fatalf("target retained a source-path dependency: %v", err)
	}
}

func TestInfrastructureArtifactCopiesRefuseChangedTargetPinBeforeWrites(t *testing.T) {
	target, other := newGenerationStore(t), newGenerationStore(t)
	pin, err := other.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = stageInfrastructureArtifactCopies(t.Context(), infrastructureRuntimeSource{}, infrastructurePairSource{}, ownerArtifactShapeRequirements(), target, pin)
	if !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("foreign target observation accepted: %v", err)
	}
	requireNoInfrastructureRuntimeArtifacts(t, target.Root)
}

func TestInfrastructureArtifactCopiesRefuseReplacedTargetBeforeWrites(t *testing.T) {
	target := newGenerationStore(t)
	pin, err := target.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Retain the original directory for Store.Close and the fixture's owned
	// cleanup. No native actor or active resource uses this store.
	retained := filepath.Join(filepath.Dir(target.Root), "retained")
	if err = os.Rename(target.Root, retained); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(target.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = stageInfrastructureArtifactCopies(t.Context(), infrastructureRuntimeSource{}, infrastructurePairSource{}, ownerArtifactShapeRequirements(), target, pin)
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("replaced target directory accepted: %v", err)
	}
	requireNoInfrastructureRuntimeArtifacts(t, target.Root)
	requireNoInfrastructureRuntimeArtifacts(t, retained)
}

func TestInfrastructureOutsideArtifactsPreserveUnboundInventorySelection(t *testing.T) {
	runner := buildInfrastructureRuntime(t)
	installedClient, installedDaemon := statePairStagingSources(t, "installation-Y-client", "installation-Y-daemon")
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-inventory", true: "interrupted-X-inventory"}[existing], func(t *testing.T) {
			target := newGenerationStore(t)
			before, expected := outsideArtifactsInventoryFixture(t, target, existing, installedClient, installedDaemon)
			artifacts, err := PrepareOutsideInfrastructureArtifacts(t.Context(), target, runner, ExecutableLookup{
				InitialExecutable: installedClient, Environment: []string{},
			}, infrastructure.InitialOperation{})
			if err != nil {
				t.Fatal(err)
			}
			if artifacts.ClientSHA256 != expected.ClientSHA256 || artifacts.DaemonSHA256 != expected.DaemonSHA256 {
				t.Fatal("outside producer replaced interrupted inventory with today's installation")
			}
			requireStatePairSelectionUnchanged(t, filepath.Join(target.Root, ".supervisor"), before, existing)
			// These exact path forms become the accepted bootstrap tuple. The
			// canonical resolver must return those same paths in both cases.
			client, daemon, err := ResolvePrivateSupervisorExecutables(target.Root, ExecutableLookup{
				InitialExecutable: artifacts.Client, Environment: []string{},
				AcceptedBootstrapPair: BootstrapPair{Client: artifacts.Client, Daemon: artifacts.Daemon},
			})
			if err != nil || client != artifacts.Client || daemon != artifacts.Daemon {
				t.Fatalf("honest accepted bootstrap path forms diverged: %v", err)
			}
			if _, found, readErr := pueue.ReadPrivateSupervisorBinding(target.Root); readErr != nil || found {
				t.Fatalf("outside content producer published a binding: found=%t err=%v", found, readErr)
			}
		})
	}
}

func outsideArtifactsInventoryFixture(t *testing.T, target *taskdir.Store, existing bool, installedClient, installedDaemon string) ([]byte, task.SupervisorRef) {
	t.Helper()
	if !existing {
		return nil, savedSupervisorPairRef(t, installedClient, installedDaemon)
	}
	client, daemon := statePairStagingSources(t, "interrupted-X-client", "interrupted-X-daemon")
	if err := installStateSupervisorPair(t.Context(), target.Root, client, daemon); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(target.Root, ".supervisor", stateSupervisorPairRecordName))
	if err != nil {
		t.Fatal(err)
	}
	return before, savedSupervisorPairRef(t, client, daemon)
}

func ownerArtifactShapeRequirements() infrastructure.ArtifactRequirements {
	return infrastructure.ArtifactRequirements{
		RequiredRuntimeSHA256: strings.Repeat("a", 64),
		RequiredClientSHA256:  strings.Repeat("b", 64),
		RequiredDaemonSHA256:  strings.Repeat("c", 64),
	}
}

func ownerArtifactRequirements(runtimeSHA string, pair task.SupervisorRef) infrastructure.ArtifactRequirements {
	return infrastructure.ArtifactRequirements{
		RequiredRuntimeSHA256: runtimeSHA,
		RequiredClientSHA256:  pair.ClientSHA256,
		RequiredDaemonSHA256:  pair.DaemonSHA256,
	}
}

// No native actor uses these synthetic fixture bindings. Production publication
// remains owned by canonical supervisor preparation, never by artifact copying.
func replaceOwnerArtifactPair(t *testing.T, root string, binding task.SupervisorRef, clientBytes, daemonBytes string) task.SupervisorRef {
	t.Helper()
	client, daemon := statePairStagingSources(t, clientBytes, daemonBytes)
	client, daemon, err := InstallStateSupervisorPairPaths(t.Context(), root, client, daemon)
	if err != nil {
		t.Fatal(err)
	}
	expected := savedSupervisorPairRef(t, client, daemon)
	binding.ClientExecutable, binding.ClientSHA256 = expected.ClientExecutable, expected.ClientSHA256
	binding.DaemonExecutable, binding.DaemonSHA256 = expected.DaemonExecutable, expected.DaemonSHA256
	if err = os.Remove(filepath.Join(root, ".supervisor", "supervisor.identity.json")); err != nil {
		t.Fatal(err)
	}
	writeHistoricalSupervisorBinding(t, root, binding)
	return binding
}

func TestInfrastructureOwnerSourceRequiresFreshPublishedPairBeforeWrites(t *testing.T) {
	sourceRoot, epochPair := infrastructureSourceFixture(t)
	candidate := buildInfrastructureRuntime(t)
	expectedRuntime, err := stateExecutableSourceDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := prepareInfrastructureRuntimeSource(t.Context(), sourceRoot, candidate)
	if err != nil {
		t.Fatal(err)
	}
	published := replaceOwnerArtifactPair(t, sourceRoot, epochPair, "published-Y-client", "published-Y-daemon")
	required := ownerArtifactRequirements(expectedRuntime, published)
	clientMismatch, daemonMismatch := required, required
	clientMismatch.RequiredClientSHA256 = epochPair.ClientSHA256
	daemonMismatch.RequiredDaemonSHA256 = epochPair.DaemonSHA256
	for _, test := range []struct {
		name     string
		required infrastructure.ArtifactRequirements
		want     error
	}{
		{"runtime", ownerArtifactRequirements(strings.Repeat("d", 64), published), infrastructure.ErrIncompatible},
		{"client", clientMismatch, infrastructure.ErrIncompatible},
		{"daemon", daemonMismatch, infrastructure.ErrIncompatible},
		{"old-epoch-pair", ownerArtifactRequirements(expectedRuntime, epochPair), infrastructure.ErrIncompatible},
		{"missing", infrastructure.ArtifactRequirements{}, infrastructure.ErrIdentity},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newGenerationStore(t)
			if _, stageErr := stageInfrastructureOwnerSource(t.Context(), runtime, test.required, target); !errors.Is(stageErr, test.want) {
				t.Fatalf("required mismatch did not refuse: %v", stageErr)
			}
			requireNoInfrastructureRuntimeArtifacts(t, target.Root)
		})
	}
	target := newGenerationStore(t)
	staged, err := stageInfrastructureOwnerSource(t.Context(), runtime, required, target)
	if err != nil {
		t.Fatal(err)
	}
	if staged.RunnerSHA256 != expectedRuntime || staged.ClientSHA256 != published.ClientSHA256 || staged.DaemonSHA256 != published.DaemonSHA256 {
		t.Fatal("fresh published pair Y did not satisfy request Y")
	}
	supervisorfixture.RequireExecutable(t, staged.Client, "published-Y-client")
	supervisorfixture.RequireExecutable(t, staged.Daemon, "published-Y-daemon")
	requireStatePairSelectionUnchanged(t, filepath.Join(target.Root, ".supervisor"), nil, false)
	if _, found, readErr := pueue.ReadPrivateSupervisorBinding(target.Root); readErr != nil || found {
		t.Fatalf("staging published target binding: found=%t err=%v", found, readErr)
	}
}

func TestInfrastructureArtifactCopiesRejectChangedSourceBeforeWrites(t *testing.T) {
	candidate := buildInfrastructureRuntime(t)
	expectedRuntime, err := stateExecutableSourceDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"runtime", "client", "daemon"} {
		t.Run(kind, func(t *testing.T) {
			root, binding := infrastructureSourceFixture(t)
			runtime, prepareErr := prepareInfrastructureRuntimeSource(t.Context(), root, candidate)
			if prepareErr != nil {
				t.Fatal(prepareErr)
			}
			pair, readErr := readInfrastructurePairSource(t.Context(), root)
			if readErr != nil {
				t.Fatal(readErr)
			}
			path := map[string]string{"runtime": runtime.executable, "client": binding.ClientExecutable, "daemon": binding.DaemonExecutable}[kind]
			if writeErr := os.WriteFile(path, []byte("corrupted retained source"), 0o700); writeErr != nil {
				t.Fatal(writeErr)
			}
			target := newGenerationStore(t)
			pin, pinErr := target.PinRootContext(t.Context())
			if pinErr != nil {
				t.Fatal(pinErr)
			}
			_, stageErr := stageInfrastructureArtifactCopies(t.Context(), runtime, pair, ownerArtifactRequirements(expectedRuntime, binding), target, pin)
			if !errors.Is(stageErr, pueue.ErrConfiguration) {
				t.Fatalf("corrupt source was not refused: %v", stageErr)
			}
			requireNoInfrastructureRuntimeArtifacts(t, target.Root)
		})
	}
}
