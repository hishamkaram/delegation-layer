package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type infrastructurePreparationGuard func(context.Context, string, func() error) error

func (g infrastructurePreparationGuard) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return g(ctx, root, start)
}

func (g infrastructurePreparationGuard) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	return g(ctx, root, commit)
}

type infrastructurePreparationWait func(string, int, <-chan struct{}) error

func (w infrastructurePreparationWait) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	return w(root, pid, done)
}

type infrastructurePreparationFixture struct {
	origin    infrastructure.Origin
	artifacts infrastructure.Artifacts
	defaulted infrastructure.LaunchContext
	selected  infrastructure.LaunchContext
}

func newInfrastructurePreparationFixture(t *testing.T) infrastructurePreparationFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := taskdir.InitStore(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	origin, err := infrastructure.NewOrigin(store.Root, store.RootID)
	if err != nil {
		t.Fatal(err)
	}
	defaulted := infrastructure.LaunchContext{
		Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS,
		ControlEnvironment: []string{"CONTROL=epoch-default"}, MaintenanceEnvironment: []string{"MAINTENANCE=epoch-default"}, SupervisorExecutable: filepath.Join(store.Root, "pueue"),
	}
	selected := defaulted.Clone()
	selected.SupervisorExecutable = filepath.Join(store.Root, "request-selected", "pueue")
	selected.ControlEnvironment = []string{"CONTROL=request-selected"}
	selected.MaintenanceEnvironment = []string{"MAINTENANCE=request-selected"}
	// These paths test composition only. No executable/native authority is
	// claimed, and no fixture process is started.
	return infrastructurePreparationFixture{
		origin: origin, defaulted: defaulted, selected: selected,
		artifacts: infrastructure.Artifacts{Runner: filepath.Join(store.Root, "runner")},
	}
}

func inertInfrastructurePreparationPorts() (infrastructure.ResidentGuards, infrastructure.DaemonWaitOwner) {
	return infrastructurePreparationGuard(func(context.Context, string, func() error) error { return nil }),
		infrastructurePreparationWait(func(string, int, <-chan struct{}) error { return nil })
}

func TestInfrastructurePreparationOptionsUseOnlySelectedContext(t *testing.T) {
	fixture := newInfrastructurePreparationFixture(t)
	guard, wait := inertInfrastructurePreparationPorts()
	options, observation, err := infrastructurePreparationOptions(fixture.origin, fixture.artifacts, fixture.selected, guard, wait)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(options.Environment, fixture.selected.ControlEnvironment) || !slices.Equal(options.PrivateMaintenanceEnvironment, fixture.selected.MaintenanceEnvironment) {
		t.Fatal("options did not use the request's selected environments")
	}
	if slices.Equal(options.Environment, fixture.defaulted.ControlEnvironment) || slices.Equal(options.PrivateMaintenanceEnvironment, fixture.defaulted.MaintenanceEnvironment) {
		t.Fatal("epoch default replaced selected context")
	}
	if options.ControlOwnerExecutable != fixture.artifacts.Runner || !options.WaitForPendingClose || observation == nil || options.DaemonStartGuard != observation || options.DaemonWaitOwner == nil {
		t.Fatal("resident options lost accepted runner, observation, wait or pending-close policy")
	}
	requireDefaultInfrastructureCallbacks(t, options)
	if observation.Disposition() != (infrastructure.Disposition{}) {
		t.Fatal("options construction claimed readiness or Start")
	}
	fixture.selected.ControlEnvironment[0], fixture.selected.MaintenanceEnvironment[0] = "MUTATED=1", "MUTATED=2"
	if options.Environment[0] != "CONTROL=request-selected" || options.PrivateMaintenanceEnvironment[0] != "MAINTENANCE=request-selected" {
		t.Fatal("options retained caller-owned environment slices")
	}
}

func TestInfrastructurePreparationOptionsUseOnlySelectedClient(t *testing.T) {
	fixture := newInfrastructurePreparationFixture(t)
	guard, wait := inertInfrastructurePreparationPorts()
	options, _, err := infrastructurePreparationOptions(fixture.origin, fixture.artifacts, fixture.selected, guard, wait)
	if err != nil {
		t.Fatal(err)
	}
	if options.RuntimeClientExecutable != fixture.selected.SupervisorExecutable || options.RuntimeClientExecutable == fixture.defaulted.SupervisorExecutable {
		t.Fatal("options lost the request's accepted installed client")
	}
}

func TestInfrastructurePreparationBindRetainsStartupHelperWithSelectedClient(t *testing.T) {
	store, _, original, owner := newOwnedSupervisorProbeTestFixture(t)
	configPath := original.Binding().ConfigPath
	base := filepath.Dir(configPath)
	clientB := filepath.Join(owner.base, "installation-b", "pueue")
	commandLog := filepath.Join(owner.base, "selected-client-commands")
	writeRunnerFixture(t, clientB, `#!/bin/sh
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
	writeRunnerFixture(t, runnerA, "#!/bin/sh\nexit 0\n")
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
	before := capturePairFaultWitness(t, base)
	configBefore := string(readSupervisorProbeTestFile(t, configPath))

	// The existing test executable implements the control-owner protocol in
	// place. This establishes that B can complete the same Bind boundary.
	optionsB, _, err := infrastructurePreparationOptions(origin, infrastructure.Artifacts{Runner: canonicalAppTestExecutable(t)}, selected, owner, owner)
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
	optionsA, observation, err := infrastructurePreparationOptions(origin, artifactsA, selected, owner, owner)
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
	assertPairFaultWitness(t, base, before)
	if string(readSupervisorProbeTestFile(t, configPath)) != configBefore || observation.Disposition() != (infrastructure.Disposition{}) {
		t.Fatal("failed helper validation changed config or claimed readiness/Start")
	}
}

func TestInfrastructurePreparationHistoricalBindingUsesAcceptedInstalledClient(t *testing.T) {
	store, _, original, owner := newOwnedSupervisorProbeTestFixture(t)
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
	options, observation, err := infrastructurePreparationOptions(origin, infrastructure.Artifacts{Runner: canonicalAppTestExecutable(t)}, selected, owner, owner)
	if err != nil {
		t.Fatal(err)
	}
	var acceptedClientObserved atomic.Bool
	options.Observer = func(event pueue.CommandEvent) {
		if len(event.Argv) > 0 && event.Argv[0] == selected.SupervisorExecutable {
			acceptedClientObserved.Store(true)
		}
	}
	before := capturePairFaultWitness(t, filepath.Dir(legacy.ConfigPath))
	configBefore := string(readSupervisorProbeTestFile(t, legacy.ConfigPath))
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
	assertPairFaultWitness(t, filepath.Dir(legacy.ConfigPath), before)
	if string(readSupervisorProbeTestFile(t, legacy.ConfigPath)) != configBefore {
		t.Fatal("read-only recovery changed saved configuration")
	}
}

func TestInfrastructurePreparationOptionsRejectInvalidSelectedContext(t *testing.T) {
	fixture := newInfrastructurePreparationFixture(t)
	guard, wait := inertInfrastructurePreparationPorts()
	for _, kind := range []string{"root", "root-id", "os", "control-nil", "maintenance-nil", "invalid-environment", "protocol"} {
		t.Run(kind, func(t *testing.T) {
			selected := invalidInfrastructureSelectedContext(fixture.selected.Clone(), kind)
			options, observation, err := infrastructurePreparationOptions(fixture.origin, fixture.artifacts, selected, guard, wait)
			if !errors.Is(err, infrastructure.ErrIdentity) || observation != nil || options.ControlOwnerExecutable != "" || options.Environment != nil || options.PrivateMaintenanceEnvironment != nil || options.SuccessfulCleanupAuthorizer != nil {
				t.Fatalf("invalid selected %s reached options composition: %v", kind, err)
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("invalid environment leaked context bytes")
			}
		})
	}
}

func invalidInfrastructureSelectedContext(selected infrastructure.LaunchContext, kind string) infrastructure.LaunchContext {
	switch kind {
	case "root":
		selected.Root = filepath.Dir(selected.Root)
	case "root-id":
		selected.RootID = strings.Repeat("e", 32)
	case "os":
		selected.OS = "darwin"
		if runtime.GOOS == "darwin" {
			selected.OS = "linux"
		}
	case "control-nil":
		selected.ControlEnvironment = nil
	case "maintenance-nil":
		selected.MaintenanceEnvironment = nil
	case "invalid-environment":
		selected.ControlEnvironment = []string{"private-value"}
	case "protocol":
		selected.Protocol = 0
	}
	return selected
}

func TestInfrastructurePreparationOptionsRequireOwnedPortsAndNamespace(t *testing.T) {
	fixture := newInfrastructurePreparationFixture(t)
	guard, wait := inertInfrastructurePreparationPorts()
	for _, test := range []struct {
		name   string
		origin infrastructure.Origin
		guard  infrastructure.ResidentGuards
		wait   infrastructure.DaemonWaitOwner
		want   error
	}{
		{"missing guard", fixture.origin, nil, wait, os.ErrInvalid},
		{"missing wait", fixture.origin, guard, nil, os.ErrInvalid},
		{"invalid namespace", infrastructure.Origin{}, guard, wait, infrastructure.ErrIdentity},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, observation, err := infrastructurePreparationOptions(test.origin, fixture.artifacts, fixture.selected, test.guard, test.wait)
			if !errors.Is(err, test.want) || observation != nil || options.DaemonStartGuard != nil || options.DaemonWaitOwner != nil || options.SuccessfulCleanupAuthorizer != nil {
				t.Fatalf("missing port/namespace reached options composition: %v", err)
			}
		})
	}
}

func TestInfrastructurePreparationOptionsForwardObservedCallbackAndWait(t *testing.T) {
	fixture := newInfrastructurePreparationFixture(t)
	postStartFault, registrationFault := errors.New("guard post-callback fault"), errors.New("wait registration fault")
	guardCalls, callbackCalls, waitCalls := 0, 0, 0
	guard := infrastructurePreparationGuard(func(ctx context.Context, root string, start func() error) error {
		guardCalls++
		if ctx != t.Context() || root != fixture.origin.Root() {
			t.Fatal("host options replaced guard context or operation root")
		}
		if err := start(); err != nil {
			return err
		}
		return postStartFault
	})
	done := make(chan struct{})
	close(done)
	wait := infrastructurePreparationWait(func(root string, pid int, completion <-chan struct{}) error {
		waitCalls++
		if root != fixture.origin.Root() || pid != 37 || completion != done {
			t.Fatal("host options replaced wait identity or completion handle")
		}
		return registrationFault
	})
	options, observation, err := infrastructurePreparationOptions(fixture.origin, fixture.artifacts, fixture.selected, guard, wait)
	if err != nil {
		t.Fatal(err)
	}
	// This exercises the installed callback adapter, not an actual native Start.
	err = options.DaemonStartGuard.GuardDaemonStart(t.Context(), fixture.origin.Root(), func() error { callbackCalls++; return nil })
	if !errors.Is(err, postStartFault) || callbackCalls != 1 || guardCalls != 1 {
		t.Fatalf("installed observation lost callback/guard result: %v", err)
	}
	if waitErr := options.DaemonWaitOwner.RegisterDaemonWait(fixture.origin.Root(), 37, done); !errors.Is(waitErr, registrationFault) || waitCalls != 1 {
		t.Fatalf("installed wait owner changed registration result: %v", waitErr)
	}
	digest, err := fixture.selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if observation.Disposition() != (infrastructure.Disposition{Kind: infrastructure.DispositionStarted, Context: infrastructure.LaunchContextReference{SHA256: digest}}) {
		t.Fatal("post-callback error erased the selected context's observed Start")
	}
}
