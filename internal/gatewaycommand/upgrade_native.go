package gatewaycommand

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspacefiles"
)

// NativeInstallation consumes existing Native public contracts. It never owns
// a supervisor operation, scheduler state, task admission or provider policy.
type NativeInstallation interface {
	Inspect(context.Context, string, string) (NativeTransition, error)
	Ensure(context.Context, string, string, NativeTransition) (NativeTransition, error)
	Verify(context.Context, string, string) error
}

// NativeMaintenance borrows the original App invocation for Native's existing
// supervisor owner. Its observation never replaces the canonical journal.
type NativeMaintenance interface {
	ValidateUpgrade(context.Context) error
	Ensure(context.Context, NativeMaintenanceRequest) (NativeTransition, error)
}

type NativeMaintenanceRequest struct {
	Root         string
	ExpectedRoot string
	OperationID  string
}

type NativeTransition struct {
	OperationID   string `json:"operation_id,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Pending       bool   `json:"pending"`
	HandoffNeeded bool   `json:"handoff_needed"`
}

func (t NativeTransition) validate() error {
	if t.OperationID != "" {
		if _, err := pueue.ParseUpgradeOperationID(t.OperationID); err != nil {
			return err
		}
	}
	if t.Pending && (t.OperationID == "" || t.HandoffNeeded) {
		return task.ErrIdentityMismatch
	}
	return nil
}

type nativeCommandRunner interface {
	Run(context.Context, string, []string) (gatewayservice.Result, error)
}

type nativeInstallation struct {
	executable        string
	runner            nativeCommandRunner
	dependencies      Dependencies
	expectedOperation string
}

func (c invocation) nativeInstallation(executable string) (NativeInstallation, error) {
	if c.dependencies.NativeInstallation != nil {
		return c.dependencies.NativeInstallation, nil
	}
	runner, err := gatewayservice.NewNativeRunner(executable, c.dependencies.Environment)
	if err != nil {
		return nil, err
	}
	return nativeInstallation{executable: executable, runner: runner, dependencies: c.dependencies, expectedOperation: c.expectedOperation}, nil
}

// This is a consumer-owned projection of additive Native JSON. Only fields
// needed for the stated proof are decoded; every used field is checked below.
type nativeEnvelope struct {
	SchemaVersion     int                                  `json:"schema_version"`
	Command           string                               `json:"command"`
	RootID            string                               `json:"root_id"`
	Error             string                               `json:"error"`
	Failure           json.RawMessage                      `json:"failure"`
	Connector         *taskdir.ConnectorContextObservation `json:"connector"`
	Providers         []provider.Description               `json:"providers"`
	Filesystem        *workspacefiles.Result               `json:"filesystem"`
	SupervisorControl *nativeSupervisorControl             `json:"supervisor_control"`
}

type nativeSupervisorControl struct {
	State               string                           `json:"state"`
	NextAction          string                           `json:"next_action"`
	InstallationBinding *installation.BindingObservation `json:"installation_binding"`
}

func (n nativeInstallation) command(ctx context.Context, args ...string) (nativeEnvelope, int, error) {
	result, err := n.runner.Run(ctx, n.executable, args)
	if err != nil {
		return nativeEnvelope{}, result.ExitCode, err
	}
	var envelope nativeEnvelope
	if err = json.Unmarshal(result.Stdout, &envelope); err != nil || envelope.SchemaVersion != 1 {
		return nativeEnvelope{}, result.ExitCode, errors.Join(ErrInstallationMigrationRequired, err)
	}
	return envelope, result.ExitCode, nil
}

func nativeCommandSucceeded(envelope nativeEnvelope, exit int, command string) bool {
	return exit == 0 && envelope.Command == command && envelope.Error == "" && (len(envelope.Failure) == 0 || string(envelope.Failure) == "null")
}

func (n nativeInstallation) Verify(ctx context.Context, root, expectedRoot string) error {
	observed, exit, err := n.command(ctx, "context", "inspect", "--root", root, "--json")
	if err != nil {
		return err
	}
	if !nativeCommandSucceeded(observed, exit, "context") || observed.Connector == nil || observed.Connector.ContractVersion != 1 {
		return ErrInstallationMigrationRequired
	}
	if observed.Connector.RootToken != expectedRoot {
		return task.ErrIdentityMismatch
	}
	for _, feature := range []string{task.WorkspaceFilesystemFeature, task.NativeExecutionOptionsFeature, task.ProjectTokenSHA256Feature, task.GuardedContinuationFeature, task.ScopedModelDiscoveryFeature} {
		if !slices.Contains(observed.Connector.Features, feature) {
			return ErrInstallationMigrationRequired
		}
	}
	providers, exit, err := n.command(ctx, "providers", "--json")
	if err != nil {
		return err
	}
	if !nativeCommandSucceeded(providers, exit, "") || providers.Providers == nil {
		// providers has no command field in its public response.
		return ErrInstallationMigrationRequired
	}
	return n.verifyScopes(ctx, root, expectedRoot)
}

func (n nativeInstallation) verifyScopes(ctx context.Context, root, expectedRoot string) (resultErr error) {
	directory, err := filepath.Abs(os.TempDir())
	if err != nil {
		return err
	}
	parent, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	name := "delegate-gateway-upgrade-scopes-" + rand.Text() + ".json"
	file, err := parent.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	identity, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close())
	}
	defer func() {
		current, statErr := parent.Lstat(name)
		removeErr := task.ErrIdentityMismatch
		if statErr == nil && os.SameFile(identity, current) {
			removeErr = parent.Remove(name)
		}
		resultErr = errors.Join(resultErr, file.Close(), statErr, removeErr)
	}()
	request := workspacefiles.Request{ExpectedRoot: expectedRoot}
	if err = json.NewEncoder(file).Encode(request); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	observed, exit, err := n.command(ctx, "filesystem", "scopes", "--request", filepath.Join(directory, name), "--root", root, "--json")
	if err != nil {
		return err
	}
	if !nativeScopesObserved(observed, exit) {
		return ErrInstallationMigrationRequired
	}
	return nil
}

func nativeScopesObserved(observed nativeEnvelope, exit int) bool {
	result := observed.Filesystem
	return nativeCommandSucceeded(observed, exit, "filesystem") && result != nil && result.SchemaVersion == workspacefiles.SchemaVersion && result.Operation == workspacefiles.Scopes && result.Scopes != nil && !result.ObservedAt.IsZero()
}

func (n nativeInstallation) inspectJournal(ctx context.Context, root, expectedRoot, operation string) (transition NativeTransition, resultErr error) {
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, root, n.dependencies.Predicates)
	if err != nil {
		return transition, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	rootToken, err := store.RootTokenContext(ctx)
	if err != nil || rootToken != expectedRoot {
		return transition, errors.Join(task.ErrIdentityMismatch, err)
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return transition, err
	}
	op := pueue.UpgradeOperationID("")
	if operation != "" {
		op, err = pueue.ParseUpgradeOperationID(operation)
		if err != nil {
			return transition, err
		}
	}
	observed, found, err := pueue.ObservePrivateUpgrade(ctx, pin, op)
	if err != nil || !found {
		return transition, err
	}
	return NativeTransition{OperationID: string(observed.OperationID), Phase: string(observed.Phase), Pending: !observed.Completed}, nil
}

func (n nativeInstallation) Inspect(ctx context.Context, root, expectedRoot string) (NativeTransition, error) {
	if n.expectedOperation != "" {
		return n.inspectExpectedOperation(ctx, root, expectedRoot)
	}
	transition, err := n.inspectJournal(ctx, root, expectedRoot, "")
	if err != nil || transition.Pending {
		return transition, err
	}
	status, exit, err := n.command(ctx, "supervisor", "status", "--root", root, "--json")
	if err != nil {
		return transition, err
	}
	if status.Command != "supervisor" || status.SupervisorControl == nil {
		return transition, ErrInstallationMigrationRequired
	}
	control := status.SupervisorControl
	if control.InstallationBinding != nil && control.InstallationBinding.HandoffAdvised() {
		transition.HandoffNeeded = true
		return transition, nil
	}
	if !nativeCommandSucceeded(status, exit, "supervisor") || !nativeSupervisorPairCurrent(control.InstallationBinding) {
		return transition, ErrInstallationMigrationRequired
	}
	return transition, nil
}

func (n nativeInstallation) inspectExpectedOperation(ctx context.Context, root, expectedRoot string) (NativeTransition, error) {
	exact, err := n.inspectJournal(ctx, root, expectedRoot, n.expectedOperation)
	if err != nil {
		return exact, err
	}
	if exact.OperationID != n.expectedOperation {
		return exact, task.ErrEvidenceFault
	}
	if !exact.Pending {
		return exact, nil
	}
	current, err := n.inspectJournal(ctx, root, expectedRoot, "")
	if err == nil && current.OperationID != exact.OperationID {
		err = task.ErrRequestConflict
	}
	return exact, err
}

func nativeSupervisorPairCurrent(binding *installation.BindingObservation) bool {
	return binding != nil && binding.Client.SelectedAvailability == installation.AvailabilityPresent && binding.Daemon.SelectedAvailability == installation.AvailabilityPresent && (binding.Relation == installation.BindingCurrent || binding.Relation == installation.BindingNotInitialized)
}

func (n nativeInstallation) Ensure(ctx context.Context, root, expectedRoot string, previous NativeTransition) (NativeTransition, error) {
	if err := previous.validate(); err != nil {
		return previous, err
	}
	if !previous.Pending && !previous.HandoffNeeded {
		return previous, nil
	}
	if n.dependencies.NativeMaintenance == nil {
		return previous, errors.New("native maintenance authority is unavailable")
	}
	request := NativeMaintenanceRequest{Root: root, ExpectedRoot: expectedRoot}
	if previous.Pending {
		request.OperationID = previous.OperationID
	}
	observed, controlErr := n.dependencies.NativeMaintenance.Ensure(ctx, request)
	return n.settleMaintenance(ctx, request, observed, controlErr)
}

func (n nativeInstallation) settleMaintenance(ctx context.Context, request NativeMaintenanceRequest, observed NativeTransition, controlErr error) (NativeTransition, error) {
	if err := observed.validate(); err != nil {
		return observed, errors.Join(controlErr, err)
	}
	operation := observed.OperationID
	if operation == "" {
		operation = request.OperationID
	}
	if request.OperationID != "" && operation != request.OperationID {
		return observed, errors.Join(controlErr, task.ErrRequestConflict)
	}
	if operation == "" {
		// Native may be busy before accepting an operation. Do not attribute
		// that observation to whichever operation a later latest-read finds.
		return observed, controlErr
	}
	current, readErr := n.inspectJournal(ctx, request.Root, request.ExpectedRoot, operation)
	if readErr != nil {
		return observed, errors.Join(controlErr, readErr)
	}
	if current.OperationID != operation {
		return observed, errors.Join(controlErr, task.ErrEvidenceFault)
	}
	if controlErr == nil && current.Pending {
		controlErr = ErrNativeUpgradePending
	}
	return current, controlErr
}
