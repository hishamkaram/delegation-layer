package supervisor

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOutsideRuntimeObservationDoesNotReadOrChangePairSelection(t *testing.T) {
	runner := buildInfrastructureRuntime(t)
	expected, err := stateExecutableSourceDigest(runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, unavailableInventory := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "unavailable-inventory"}[unavailableInventory], func(t *testing.T) {
			target := newGenerationStore(t)
			if unavailableInventory {
				client, daemon := statePairStagingSources(t, "old-client", "old-daemon")
				installed, _, installErr := InstallStateSupervisorPairPaths(t.Context(), target.Root, client, daemon)
				if installErr != nil {
					t.Fatal(installErr)
				}
				if removeErr := os.Remove(installed); removeErr != nil {
					t.Fatal(removeErr)
				}
			}
			before := outsideRequirementsTree(t, target.Root)
			got, observeErr := ObserveOutsideInfrastructureRuntime(t.Context(), target, runner)
			if observeErr != nil || got != expected {
				t.Fatalf("caller runtime depended on an obsolete pair: digest=%q error=%v", got, observeErr)
			}
			if !maps.Equal(before, outsideRequirementsTree(t, target.Root)) {
				t.Fatal("runtime observation staged or changed target state")
			}
		})
	}
}

func TestOutsideRuntimeObservationRefusesInvalidOrCanceledSources(t *testing.T) {
	target := newGenerationStore(t)
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	before := outsideRequirementsTree(t, target.Root)
	if _, err = ObserveOutsideInfrastructureRuntime(t.Context(), target, runner); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("unmanaged caller runtime accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = ObserveOutsideInfrastructureRuntime(ctx, target, filepath.Join(target.Root, "missing-runner")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost before opening missing runtime: %v", err)
	}
	var missingContext context.Context // Exercise the API's rejected zero-context input.
	if _, err = ObserveOutsideInfrastructureRuntime(missingContext, target, runner); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil invocation context accepted: %v", err)
	}
	if !maps.Equal(before, outsideRequirementsTree(t, target.Root)) {
		t.Fatal("refused runtime observation changed target state")
	}
}

func TestOutsideRequirementsObserveInventoryWithoutPreparingTarget(t *testing.T) {
	runner := buildInfrastructureRuntime(t)
	runtimeSHA, err := stateExecutableSourceDigest(runner)
	if err != nil {
		t.Fatal(err)
	}
	client, daemon := statePairStagingSources(t, "observed-installation-client", "observed-installation-daemon")
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "interrupted-inventory"}[existing], func(t *testing.T) {
			target := newGenerationStore(t)
			_, expected := outsideArtifactsInventoryFixture(t, target, existing, client, daemon)
			before := outsideRequirementsTree(t, target.Root)
			lookup := ExecutableLookup{InitialExecutable: client, Environment: []string{}}
			observed, readErr := ObserveOutsideInfrastructureRequirements(t.Context(), target, runner, lookup, infrastructure.InitialOperation{})
			if readErr != nil {
				t.Fatal(readErr)
			}
			if observed != ownerArtifactRequirements(runtimeSHA, expected) {
				t.Fatal("observation selected a different source tuple")
			}
			if !maps.Equal(before, outsideRequirementsTree(t, target.Root)) {
				t.Fatal("requirements observation prepared or changed target content")
			}
			staged, stageErr := PrepareOutsideInfrastructureArtifacts(t.Context(), target, runner, lookup, infrastructure.InitialOperation{})
			if stageErr != nil {
				t.Fatal(stageErr)
			}
			if checkErr := observed.ValidateArtifacts(staged); checkErr != nil {
				t.Fatal(checkErr)
			}
		})
	}
}

func TestOutsideRequirementsRefuseUnmanagedRunnerWithoutTargetWrites(t *testing.T) {
	target := newGenerationStore(t)
	client, _ := statePairStagingSources(t, "requirement-client", "requirement-daemon")
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	before := outsideRequirementsTree(t, target.Root)
	if _, err = ObserveOutsideInfrastructureRequirements(t.Context(), target, runner, ExecutableLookup{InitialExecutable: client, Environment: []string{}}, infrastructure.InitialOperation{}); err == nil {
		t.Fatal("unmanaged test image accepted")
	}
	if !maps.Equal(before, outsideRequirementsTree(t, target.Root)) {
		t.Fatal("refused observation changed target")
	}
	missing := filepath.Join(target.Root, "missing-runner")
	if _, err = ObserveOutsideInfrastructureRequirements(t.Context(), target, missing, ExecutableLookup{InitialExecutable: client, Environment: []string{}}, infrastructure.InitialOperation{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source cause lost: %v", err)
	}
}

type outsideRequirementsEntry struct {
	mode         fs.FileMode
	sha256       string
	modification time.Time
}

func outsideRequirementsTree(t *testing.T, root string) map[string]outsideRequirementsEntry {
	t.Helper()
	entries := make(map[string]outsideRequirementsEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		observed := outsideRequirementsEntry{mode: info.Mode(), modification: info.ModTime()}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			observed.sha256 = task.ComputeSHA256(data)
		}
		entries[path] = observed
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestOutsideManualMigrationSelectsCurrentInstallationWithoutPublishing(t *testing.T) {
	runner := buildInfrastructureRuntime(t)
	target := newGenerationStore(t)
	client, daemon := statePairStagingSources(t, "manual-Y-client", "manual-Y-daemon")
	before, oldPair := outsideArtifactsInventoryFixture(t, target, true, client, daemon)
	current := savedSupervisorPairRef(t, client, daemon)
	if oldPair.ClientSHA256 == current.ClientSHA256 || oldPair.DaemonSHA256 == current.DaemonSHA256 {
		t.Fatal("fixture generations must differ")
	}
	lookup := ExecutableLookup{InitialExecutable: client, Environment: []string{}}
	initial := infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}
	snapshot := outsideRequirementsTree(t, target.Root)
	requirements, err := ObserveOutsideInfrastructureRequirements(t.Context(), target, runner, lookup, initial)
	if err != nil {
		t.Fatal(err)
	}
	if requirements.RequiredClientSHA256 != current.ClientSHA256 || requirements.RequiredDaemonSHA256 != current.DaemonSHA256 {
		t.Fatal("manual migration selected stale inventory instead of current installation")
	}
	if !maps.Equal(snapshot, outsideRequirementsTree(t, target.Root)) {
		t.Fatal("manual requirements observation changed target")
	}
	staged, err := PrepareOutsideInfrastructureArtifacts(t.Context(), target, runner, lookup, initial)
	if err != nil {
		t.Fatal(err)
	}
	if err = requirements.ValidateArtifacts(staged); err != nil {
		t.Fatalf("manual observation and preparation disagree: %v", err)
	}
	requireStatePairSelectionUnchanged(t, filepath.Join(target.Root, ".supervisor"), before, true)
	if _, found, readErr := pueue.ReadPrivateSupervisorBinding(target.Root); readErr != nil || found {
		t.Fatalf("manual content preparation published active binding: found=%t err=%v", found, readErr)
	}
}

func TestOutsideArtifactsRefuseInvalidOperationBeforePreparingTarget(t *testing.T) {
	target := newGenerationStore(t)
	lookup := ExecutableLookup{InitialExecutable: filepath.Join(target.Root, "missing-client"), Environment: []string{}}
	for _, initial := range []infrastructure.InitialOperation{
		{Kind: "invalid"},
		{Kind: infrastructure.InitialMigrateOwn, Source: infrastructure.SavedBindingSource{RootID: target.RootID}},
	} {
		before := outsideRequirementsTree(t, target.Root)
		_, observeErr := ObserveOutsideInfrastructureRequirements(t.Context(), target, "missing-runner", lookup, initial)
		_, prepareErr := PrepareOutsideInfrastructureArtifacts(t.Context(), target, "missing-runner", lookup, initial)
		if !errors.Is(observeErr, infrastructure.ErrIdentity) || !errors.Is(prepareErr, infrastructure.ErrIdentity) {
			t.Fatalf("invalid operation reached source lookup: observe=%v prepare=%v", observeErr, prepareErr)
		}
		if !maps.Equal(before, outsideRequirementsTree(t, target.Root)) {
			t.Fatal("invalid operation prepared target content")
		}
	}
}
