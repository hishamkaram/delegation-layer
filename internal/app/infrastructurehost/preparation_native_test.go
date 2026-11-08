package infrastructurehost

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInfrastructurePreparationBindRetainsStartupHelperWithSelectedClient(t *testing.T) {
	store, original, owner := newOwnedPreparationProbe(t)
	configPath := original.Binding().ConfigPath
	base := filepath.Dir(configPath)
	clientB := filepath.Join(owner.base, "installation-b", "pueue")
	commandLog := filepath.Join(owner.base, "selected-client-commands")
	supervisorfixture.WriteExecutable(t, clientB, `#!/bin/sh
set -eu
[ "$#" -eq 3 ]
[ "$1" = -c ]
[ "$3" = --version ]
printf '%s\n' "$3" >> "$FIXTURE_COMMAND_LOG"
printf '%s\n' 'pueue 4.0.4'
`)
	if err := os.WriteFile(commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runnerA := filepath.Join(owner.base, "installation-a", "delegate-run")
	supervisorfixture.WriteExecutable(t, runnerA, "#!/bin/sh\nexit 0\n")
	// The startup snapshot remains A even after that installation is removed.
	artifactsA := infrastructure.Artifacts{Runner: runnerA}
	if err := os.Remove(runnerA); err != nil {
		t.Fatal(err)
	}
	origin, err := infrastructure.NewOrigin(store.Root, store.RootID)
	if err != nil {
		t.Fatal(err)
	}
	selected := infrastructure.LaunchContext{
		Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS,
		SupervisorExecutable: clientB, ControlEnvironment: []string{"FIXTURE_COMMAND_LOG=" + commandLog}, MaintenanceEnvironment: []string{},
	}
	before := supervisorfixture.CapturePairFaultWitness(t, base)
	configBefore := string(readPreparationProbeFile(t, configPath))

	// The existing test executable implements the control-owner protocol in
	// place. This establishes that B can complete the same Bind boundary.
	optionsB, _, err := infrastructurePreparationOptions(origin, infrastructure.Artifacts{Runner: canonicalTestExecutable(t)}, selected, owner, owner, refusingCallbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	boundB, err := pueue.Bind(t.Context(), selected.SupervisorExecutable, configPath, optionsB)
	owner.ownClient(boundB, err)
	if err != nil {
		t.Fatalf("bind selected B with available helper: %v", err)
	}
	assertOrdinarySubmissionCommandCount(t, commandLog, "--version", 1)
	if err = os.WriteFile(commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	optionsA, observation, err := infrastructurePreparationOptions(origin, artifactsA, selected, owner, owner, refusingCallbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	boundA, err := pueue.Bind(t.Context(), selected.SupervisorExecutable, configPath, optionsA)
	owner.ownClient(boundA, err)
	var pathErr *os.PathError
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, pueue.ErrBinding) || !errors.As(err, &pathErr) || pathErr.Path != runnerA {
		t.Fatalf("Bind with selected B lost missing startup helper A cause: %v", err)
	}
	assertOrdinarySubmissionCommandCount(t, commandLog, "--version", 0)
	supervisorfixture.AssertPairFaultWitness(t, base, before)
	if string(readPreparationProbeFile(t, configPath)) != configBefore || observation.Disposition() != (infrastructure.Disposition{}) {
		t.Fatal("failed helper validation changed config or claimed readiness/Start")
	}
}

func TestInfrastructurePreparationHistoricalBindingUsesAcceptedInstalledClient(t *testing.T) {
	store, original, owner := newOwnedPreparationProbe(t)
	wantRuntime := original.RuntimeBinding()
	legacy := original.Binding()
	legacyDirectory := filepath.Join(store.Root, ".supervisor", "pueue-pair-"+strings.Repeat("a", 64))
	legacy.ClientExecutable = filepath.Join(legacyDirectory, "pueue")
	legacy.DaemonExecutable = filepath.Join(legacyDirectory, "pueued")
	legacy.ExecutableIdentity = ""
	// Historical reference data only: neither old program is created or hashed.
	legacy.ClientSHA256, legacy.DaemonSHA256 = strings.Repeat("b", 64), strings.Repeat("c", 64)
	if err := task.ValidateFreshSupervisorRef(legacy); err != nil {
		t.Fatal(err)
	}
	if !pueue.IsLegacyStateSupervisorClient(store.Root, legacy) {
		t.Fatal("historical copied-client precondition not established")
	}
	origin, err := infrastructure.NewOrigin(store.Root, store.RootID)
	if err != nil {
		t.Fatal(err)
	}
	selected := infrastructure.LaunchContext{
		Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS,
		SupervisorExecutable: wantRuntime.ClientExecutable,
		ControlEnvironment:   pueue.DefaultEnvironment(), MaintenanceEnvironment: []string{},
	}
	options, observation, err := infrastructurePreparationOptions(origin, infrastructure.Artifacts{Runner: canonicalTestExecutable(t)}, selected, owner, owner, refusingCallbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	var acceptedClientObserved atomic.Bool
	options.Observer = func(event pueue.CommandEvent) {
		if len(event.Argv) > 0 && event.Argv[0] == selected.SupervisorExecutable {
			acceptedClientObserved.Store(true)
		}
	}
	before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(legacy.ConfigPath))
	configBefore := string(readPreparationProbeFile(t, legacy.ConfigPath))
	client, err := pueue.BindSaved(t.Context(), legacy, options)
	owner.ownClient(client, err)
	if err != nil {
		t.Fatalf("accepted installed client could not recover historical binding: %v", err)
	}
	if err = client.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if client.Binding() != legacy || client.RuntimeBinding() != wantRuntime {
		t.Fatal("recovery changed historical authority or lost the accepted installed pair")
	}
	if !acceptedClientObserved.Load() || observation.Disposition() != (infrastructure.Disposition{}) {
		t.Fatal("recovery skipped installed transport or claimed a daemon Start")
	}
	if _, err = os.Stat(legacyDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical executable directory was created: %v", err)
	}
	supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(legacy.ConfigPath), before)
	if string(readPreparationProbeFile(t, legacy.ConfigPath)) != configBefore {
		t.Fatal("read-only recovery changed saved configuration")
	}
}
