package gatewaycommand

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspacefiles"
)

type upgradePublicRunner struct {
	root, rootToken, executable string
	features                    []string
	calls                       [][]string
	scopesFailure               bool
	requestPath                 string
	binding                     installation.BindingObservation
}

func (r *upgradePublicRunner) Run(ctx context.Context, executable string, args []string) (gatewayservice.Result, error) {
	if err := ctx.Err(); err != nil {
		return gatewayservice.Result{}, err
	}
	if executable != r.executable {
		return gatewayservice.Result{}, task.ErrIdentityMismatch
	}
	r.calls = append(r.calls, slices.Clone(args))
	value := nativeEnvelope{SchemaVersion: 1}
	result := gatewayservice.Result{}
	switch args[0] {
	case "context":
		if !slices.Equal(args, []string{"context", "inspect", "--root", r.root, "--json"}) {
			return result, task.ErrIdentityMismatch
		}
		value.Command = "context"
		value.Connector = &taskdir.ConnectorContextObservation{ContractVersion: 1, RootToken: r.rootToken, Features: r.features}
	case "providers":
		if !slices.Equal(args, []string{"providers", "--json"}) {
			return result, task.ErrIdentityMismatch
		}
		value.Providers = []provider.Description{}
	case "filesystem":
		var err error
		value, result.ExitCode, err = r.filesystem(args)
		if err != nil {
			return result, err
		}
	case "supervisor":
		if !slices.Equal(args, []string{"supervisor", "status", "--root", r.root, "--json"}) {
			return result, task.ErrIdentityMismatch
		}
		value.Command = "supervisor"
		value.SupervisorControl = &nativeSupervisorControl{State: "current", InstallationBinding: &r.binding}
	default:
		return result, task.ErrIdentityMismatch
	}
	var err error
	result.Stdout, err = json.Marshal(value)
	return result, err
}

func (r *upgradePublicRunner) filesystem(args []string) (nativeEnvelope, int, error) {
	if len(args) != 7 || !slices.Equal(args[:3], []string{"filesystem", "scopes", "--request"}) || !slices.Equal(args[4:], []string{"--root", r.root, "--json"}) {
		return nativeEnvelope{}, 0, task.ErrIdentityMismatch
	}
	r.requestPath = args[3]
	if err := r.checkRequest(); err != nil {
		return nativeEnvelope{}, 0, err
	}
	value := nativeEnvelope{SchemaVersion: 1, Command: "filesystem"}
	if r.scopesFailure {
		value.Failure = json.RawMessage(`{"code":"unsupported"}`)
		return value, 1, nil
	}
	value.Filesystem = &workspacefiles.Result{SchemaVersion: 1, Operation: workspacefiles.Scopes, ObservedAt: time.Now(), Scopes: &workspacefiles.ScopesResult{Scopes: []workspacefiles.ObservedScope{}}}
	return value, 0, nil
}

func (r *upgradePublicRunner) checkRequest() error {
	info, err := os.Stat(r.requestPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	data, err := os.ReadFile(r.requestPath)
	if err != nil {
		return err
	}
	var request workspacefiles.Request
	if err = task.DecodeStrict(data, &request); err != nil || request.ExpectedRoot != r.rootToken || request.Scope != (workspacefiles.Scope{}) {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	return nil
}

func upgradePublicFixture(t *testing.T) (nativeInstallation, *upgradePublicRunner) {
	t.Helper()
	root, observed, err := rootContext(boundedTestContext(t), filepath.Join(t.TempDir(), "state"), commandDependencies(t, &commandClient{}).Predicates, true)
	if err != nil {
		t.Fatal(err)
	}
	runner := &upgradePublicRunner{root: root, rootToken: observed.RootToken, executable: "/selected/delegate", features: []string{task.WorkspaceFilesystemFeature, task.NativeExecutionOptionsFeature, task.ProjectTokenSHA256Feature, task.GuardedContinuationFeature, task.ScopedModelDiscoveryFeature}, binding: installation.BindingObservation{Relation: installation.BindingCurrent, Client: installation.RoleBindingObservation{SelectedAvailability: installation.AvailabilityPresent}, Daemon: installation.RoleBindingObservation{SelectedAvailability: installation.AvailabilityPresent}}}
	return nativeInstallation{executable: runner.executable, runner: runner, dependencies: commandDependencies(t, &commandClient{})}, runner
}

func TestGatewayUpgradePublicPreflightRequiresFeaturesAndFunctionalScopes(t *testing.T) {
	for _, missing := range []string{"none", task.WorkspaceFilesystemFeature, task.NativeExecutionOptionsFeature, task.ProjectTokenSHA256Feature, task.GuardedContinuationFeature, task.ScopedModelDiscoveryFeature, "functional scopes"} {
		t.Run(missing, func(t *testing.T) { checkUpgradePublicPreflight(t, missing) })
	}
}

func checkUpgradePublicPreflight(t *testing.T, missing string) {
	t.Helper()
	native, runner := upgradePublicFixture(t)
	if missing == "functional scopes" {
		runner.scopesFailure = true
	} else {
		runner.features = slices.DeleteFunc(runner.features, func(value string) bool { return value == missing })
	}
	err := native.Verify(boundedTestContext(t), runner.root, runner.rootToken)
	if missing == "none" {
		if err != nil || len(runner.calls) != 3 {
			t.Fatalf("compatible public preflight failed: calls=%v err=%v", runner.calls, err)
		}
	} else if !errors.Is(err, ErrInstallationMigrationRequired) {
		t.Fatalf("missing %s was accepted: %v", missing, err)
	}
	if runner.requestPath != "" {
		if _, statErr := os.Lstat(runner.requestPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("owned preflight request was retained: %v", statErr)
		}
	}
}

func TestGatewayUpgradeReadySupervisorNeedsNoNewOperation(t *testing.T) {
	native, runner := upgradePublicFixture(t)
	transition, err := native.Inspect(boundedTestContext(t), runner.root, runner.rootToken)
	if err != nil || transition.Pending || transition.HandoffNeeded {
		t.Fatalf("ready supervisor requested handoff: %+v %v", transition, err)
	}
	after, err := native.Ensure(boundedTestContext(t), runner.root, runner.rootToken, transition)
	if err != nil || after != transition || !reflect.DeepEqual(runner.calls, [][]string{{"supervisor", "status", "--root", runner.root, "--json"}}) {
		t.Fatalf("ready supervisor caused mutation: %+v %v", runner.calls, err)
	}
}

func TestGatewayUpgradeOutdatedBindingAloneAdvisesExistingOwner(t *testing.T) {
	native, runner := upgradePublicFixture(t)
	runner.binding.Relation = installation.BindingDifferent
	transition, err := native.Inspect(boundedTestContext(t), runner.root, runner.rootToken)
	if err != nil || !transition.HandoffNeeded || transition.Pending || transition.OperationID != "" {
		t.Fatalf("actual outdated binding was ignored: %+v %v", transition, err)
	}
	// A different connector executable alone is not this source observation.
	runner.binding.Relation = installation.BindingCurrent
	runner.executable = "/other/selected/delegate"
	native.executable = runner.executable
	transition, err = native.Inspect(boundedTestContext(t), runner.root, runner.rootToken)
	if err != nil || transition.HandoffNeeded {
		t.Fatalf("connector pathname invented supervisor drift: %+v %v", transition, err)
	}
}

func TestGatewayUpgradeNativeGuardRejectsDifferentOriginalRoot(t *testing.T) {
	native, runner := upgradePublicFixture(t)
	_, err := native.Inspect(boundedTestContext(t), runner.root, strings.Repeat("0", 64))
	if !errors.Is(err, task.ErrIdentityMismatch) || len(runner.calls) != 0 {
		t.Fatalf("different root reached public supervisor control: calls=%v err=%v", runner.calls, err)
	}
	if err = native.Verify(boundedTestContext(t), runner.root, strings.Repeat("0", 64)); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("public context root mismatch became installation advice: %v", err)
	}
}

func TestGatewayUpgradeUninitializedSupervisorStillRequiresInstalledPair(t *testing.T) {
	native, runner := upgradePublicFixture(t)
	runner.binding.Relation = installation.BindingNotInitialized
	runner.binding.Daemon.SelectedAvailability = installation.AvailabilityMissing
	transition, err := native.Inspect(boundedTestContext(t), runner.root, runner.rootToken)
	if !errors.Is(err, ErrInstallationMigrationRequired) || transition.HandoffNeeded {
		t.Fatalf("missing selected pair was declared compatible: %+v %v", transition, err)
	}
}
