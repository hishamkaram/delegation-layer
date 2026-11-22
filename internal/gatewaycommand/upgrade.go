package gatewaycommand

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	ErrInstallationMigrationRequired = errors.New("selected Native installation requires explicit migration")
	ErrNativeUpgradePending          = errors.New("native installation upgrade is pending")
)

// UpgradeObservation is invocation-local progress, never a second journal.
// SDK profile, OS service and Native supervisor owners retain durable state.
type UpgradeObservation struct {
	Phase                   string                               `json:"phase"`
	Profile                 deviceclient.ProfileMigrationPreview `json:"profile"`
	Native                  NativeInstallationObservation        `json:"native"`
	Supervisor              NativeTransition                     `json:"supervisor"`
	DeviceIdentityPreserved bool                                 `json:"device_identity_preserved"`
	// ServiceRestarted is a managed-service start observation. Online and
	// execution readiness remain independent, dated observations.
	ServiceRestarted bool `json:"service_restarted"`
}

type NativeInstallationObservation struct {
	// SavedExecutable is the source read at invocation start. A failed commit
	// must be inspected through the SDK; this projection never implies rollback.
	SavedExecutable      string `json:"saved_executable"`
	SelectedExecutable   string `json:"selected_executable"`
	NativeRoot           string `json:"native_root"`
	RootContextMatches   bool   `json:"root_context_matches"`
	CapabilitiesVerified bool   `json:"capabilities_verified"`
	Verification         string `json:"verification"`
}

func nativeUpgradePendingOnly(err error) bool {
	return errors.Is(err, ErrNativeUpgradePending) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, ErrNativeUpgradePending)
	})
}

func (c invocation) status(ctx context.Context, response Response) (Response, error) {
	status, err := c.dependencies.Client.Status(ctx, c.profile)
	response.Credential = &status
	if err != nil {
		return response, err
	}
	_, err = c.selectedContext(ctx)
	return response, err
}

func (c invocation) upgrade(ctx context.Context, response Response) (Response, error) {
	response.Upgrade = &UpgradeObservation{Phase: "preview"}
	preview, err := c.dependencies.Client.PreviewProfileMigration(ctx, c.profile)
	response.Upgrade.Profile = preview
	if err != nil {
		return response, err
	}
	local, err := c.dependencies.Client.ProfileMigrationSource(ctx, c.profile)
	if err != nil {
		return response, err
	}
	executable, err := c.currentExecutable()
	if err != nil {
		return response, errors.Join(ErrInstallationMigrationRequired, err)
	}
	response.Upgrade.Native = NativeInstallationObservation{SavedExecutable: local.DelegateExecutable, SelectedExecutable: executable, NativeRoot: local.DelegateRoot, Verification: "not_run"}
	if c.arguments.Preview {
		return response, nil
	}
	if !c.arguments.AcceptExecutionPolicyRemoval {
		return response, deviceclient.ErrMigrationAcceptanceRequired
	}
	response.Upgrade.Phase = "quiesce_connector"
	manager, err := c.serviceManager(local.DelegateExecutable)
	if err != nil {
		return response, err
	}
	stopped, err := manager.Uninstall(ctx)
	response.Service = &stopped
	if err != nil {
		return response, err
	}
	response.Upgrade.Phase = "migrate_installation"
	err = gatewayservice.WithProfileLock(ctx, c.profile, func() error {
		return c.applyUpgrade(ctx, local, executable, response.Upgrade, &response)
	})
	if err != nil {
		return response, err
	}
	response.Upgrade.Phase = "restart_connector"
	selectedManager, err := c.serviceManager(executable)
	if err != nil {
		return response, err
	}
	response.Upgrade.Phase = "install_service"
	installed, err := selectedManager.Install(ctx)
	response.Service = &installed
	if err != nil {
		return response, err
	}
	response.Upgrade.Phase = "start_service"
	started, err := selectedManager.Start(ctx)
	response.Service = &started
	if err != nil {
		return response, err
	}
	if !startedServiceObserved(started) {
		return response, gatewayservice.ErrManager
	}
	response.Upgrade.ServiceRestarted = true
	response.Upgrade.Phase = "complete"
	return response, nil
}

func startedServiceObserved(status gatewayservice.Status) bool {
	return status.Installed && (status.State == gatewayservice.StateRunning || status.State == gatewayservice.StateRegistered)
}

// The Native profile-directory lock excludes supported serve/service startups.
// SDK writers independently own profile and identity leases through each commit.
func (c invocation) applyUpgrade(ctx context.Context, local deviceclient.LocalContextInfo, executable string, progress *UpgradeObservation, response *Response) error {
	inactive, err := c.dependencies.Client.RuntimeInactive(ctx, c.profile)
	if err != nil {
		return err
	}
	if !inactive {
		return gatewayservice.ErrPending
	}
	before, err := c.dependencies.Client.Status(ctx, c.profile)
	if err != nil {
		return err
	}
	if before.IdentityInUse || before.DeviceID == "" || before.CredentialGeneration == 0 {
		return deviceclient.ErrEnrollment
	}
	if err = c.prepareUpgradeNative(ctx, local, executable, progress); err != nil {
		return err
	}
	return c.commitUpgradeSelection(ctx, local, executable, before, progress, response)
}

func (c invocation) prepareUpgradeNative(ctx context.Context, local deviceclient.LocalContextInfo, executable string, progress *UpgradeObservation) error {
	native, err := c.nativeInstallation(executable)
	if err != nil {
		return err
	}
	progress.Phase = "verify_selected_installation"
	if err = native.Verify(ctx, local.DelegateRoot, local.DelegateRootContext); err != nil {
		return err
	}
	transition, err := native.Inspect(ctx, local.DelegateRoot, local.DelegateRootContext)
	progress.Supervisor = transition
	if err != nil {
		return err
	}
	if err = transition.validate(); err != nil {
		return err
	}
	if transition.Pending || transition.HandoffNeeded {
		progress.Phase = "supervisor_transition"
		selectedTransition := transition
		transition, err = native.Ensure(ctx, local.DelegateRoot, local.DelegateRootContext, transition)
		progress.Supervisor = transition
		if err != nil {
			return err
		}
		if err = transition.validate(); err != nil {
			return err
		}
		if selectedTransition.Pending && transition.OperationID != selectedTransition.OperationID {
			return task.ErrRequestConflict
		}
		if transition.Pending {
			return ErrNativeUpgradePending
		}
		progress.Phase = "verify_selected_installation"
		if err = native.Verify(ctx, local.DelegateRoot, local.DelegateRootContext); err != nil {
			return err
		}
	}
	progress.Native.RootContextMatches, progress.Native.CapabilitiesVerified = true, true
	progress.Native.Verification = "guarded_public_commands"
	return nil
}

func (c invocation) commitUpgradeSelection(ctx context.Context, local deviceclient.LocalContextInfo, executable string, before deviceclient.ConnectionStatus, progress *UpgradeObservation, response *Response) error {
	progress.Phase = "profile_migration"
	migrated, err := c.dependencies.Client.MigrateProfile(ctx, deviceclient.ProfileMigrationOptions{ProfileDirectory: c.profile, AcceptExecutionPolicyRemoval: true})
	progress.Profile = migrated
	if err != nil {
		return err
	}
	progress.Phase = "installation_selection"
	expected := local.InstallationSelection()
	selected := expected
	selected.DelegateExecutable = executable
	if err = c.dependencies.Client.UpdateInstallationSelection(ctx, c.profile, expected, selected); err != nil {
		return err
	}
	after, err := c.dependencies.Client.Status(ctx, c.profile)
	response.Credential = &after
	if err != nil {
		return err
	}
	if after.IdentityInUse || !sameCredentialIdentity(before, after) {
		return deviceclient.ErrConflict
	}
	progress.DeviceIdentityPreserved = true
	return nil
}

func sameCredentialIdentity(a, b deviceclient.ConnectionStatus) bool {
	return a.GatewayID == b.GatewayID && a.GatewayURL == b.GatewayURL && a.DeviceID == b.DeviceID && a.OwnerPrincipal == b.OwnerPrincipal && a.CredentialGeneration == b.CredentialGeneration && a.CertificateExpiresAt.Equal(b.CertificateExpiresAt)
}
