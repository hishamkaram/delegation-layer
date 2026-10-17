package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PreparePrivateAdmissionSupervisor(initialSupervisorExecutable string, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	controlContext, cancel := ControlContext(options)
	defer cancel()
	return PreparePrivateAdmissionSupervisorContext(controlContext, initialSupervisorExecutable, store, options)
}

func PreparePrivateAdmissionSupervisorContext(ctx context.Context, initialSupervisorExecutable string, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if store == nil {
		return nil, errors.New("missing inspection state store")
	}
	for {
		if supervisor, err := ProbePrivateAdmissionSupervisor(ctx, store, "", "", options); err != nil || supervisor != nil {
			return supervisor, err
		}
		supervisor, acquired, transitionErr := electPrivateAdmissionTransition(ctx, initialSupervisorExecutable, store, options)
		if transitionErr != nil {
			return nil, ClosePrivateAdmissionTransition(supervisor, transitionErr)
		}
		if acquired {
			if supervisor == nil {
				return nil, pueue.ErrConfiguration
			}
			return supervisor, nil
		}
		if err := pueue.WaitForPrivateTransition(ctx, store.Root); err != nil {
			return nil, err
		}
	}
}

func electPrivateAdmissionTransition(ctx context.Context, initialSupervisorExecutable string, store *taskdir.Store, options pueue.Options) (*pueue.Client, bool, error) {
	var supervisor *pueue.Client
	acquired, err := pueue.TryWithPrivateTransition(ctx, store.Root, func() error {
		var probeErr error
		supervisor, probeErr = ProbePrivateAdmissionSupervisor(ctx, store, "", "", options)
		if probeErr != nil || supervisor != nil {
			return probeErr
		}
		return pueue.WithBootstrapLockContext(ctx, store.Root, func(lockedContext context.Context) error {
			var prepareErr error
			supervisor, prepareErr = PreparePrivateAdmissionTransition(lockedContext, initialSupervisorExecutable, store, options)
			return prepareErr
		})
	})
	return supervisor, acquired, err
}

func PreparePrivateAdmissionTransition(ctx context.Context, initialSupervisorExecutable string, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	configExisted, err := privateSupervisorConfigExisted(store.Root)
	if err != nil {
		return nil, err
	}
	if err = pueue.EnsurePrivateTaskObservationWithBootstrapLock(store.Root); err != nil {
		return nil, err
	}
	if err = ReconcilePublishedSupervisorWithLock(ctx, store, options, false); err != nil {
		return nil, err
	}
	supervisor, ready, err := repairLivePrivateAdmissionTransition(ctx, store, options, configExisted)
	if err != nil || ready {
		return supervisor, err
	}
	supervisor, published, err := pueue.PreparePublishedPrivateWithLock(ctx, store.Root, options)
	if err != nil {
		return nil, err
	}
	if published {
		return restartPublishedPrivateAdmission(ctx, store, supervisor, configExisted)
	}
	return bootstrapPrivateAdmission(ctx, initialSupervisorExecutable, store, options, configExisted)
}

func repairLivePrivateAdmissionTransition(ctx context.Context, store *taskdir.Store, options pueue.Options, configExisted bool) (*pueue.Client, bool, error) {
	supervisor, groups, ready, err := pueue.ProbePrivateGroupsWithBootstrapLock(ctx, "", "", store.Root, options)
	if err != nil || !ready {
		return supervisor, ready, err
	}
	if err = resolvePrivateAdmissionMigrationWithBootstrapLock(ctx, store, supervisor, configExisted, true); err != nil {
		return supervisor, true, err
	}
	configured, err := OrdinarySchedulingConfiguredGroups(store.Root, supervisor.Binding(), groups)
	if err != nil || configured {
		return supervisor, true, err
	}
	err = RepairOrdinarySchedulingWithBootstrapLock(ctx, store.Root, supervisor)
	return supervisor, true, err
}

func restartPublishedPrivateAdmission(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted bool) (*pueue.Client, error) {
	if supervisor == nil {
		return nil, pueue.ErrConfiguration
	}
	if err := resolvePrivateAdmissionMigrationWithBootstrapLock(ctx, store, supervisor, configExisted, false); err != nil {
		return supervisor, err
	}
	if err := supervisor.StartPrivate(ctx, store.Root); err != nil {
		return supervisor, err
	}
	return supervisor, EnsureOrdinarySchedulingWithBootstrapLock(ctx, store.Root, supervisor)
}

func bootstrapPrivateAdmission(ctx context.Context, initialSupervisorExecutable string, store *taskdir.Store, options pueue.Options, configExisted bool) (*pueue.Client, error) {
	clientExecutable, daemonExecutable, err := ResolvePrivateSupervisorExecutables(store.Root, initialSupervisorExecutable)
	if err != nil {
		return nil, err
	}
	supervisor, err := pueue.PreparePrivateWithLock(ctx, clientExecutable, daemonExecutable, store.Root, options)
	if err != nil {
		return nil, err
	}
	if err = resolvePrivateAdmissionMigrationWithBootstrapLock(ctx, store, supervisor, configExisted, false); err != nil {
		return supervisor, err
	}
	if err = supervisor.StartPrivate(ctx, store.Root); err != nil {
		return supervisor, err
	}
	if err = supervisor.EnsurePrivateSupervisorBindingWithLock(ctx, store.Root); err != nil {
		return supervisor, err
	}
	return supervisor, EnsureOrdinarySchedulingWithBootstrapLock(ctx, store.Root, supervisor)
}

func ClosePrivateAdmissionTransition(supervisor *pueue.Client, err error) error {
	if supervisor == nil {
		return err
	}
	return errors.Join(err, supervisor.CloseAfterPending(err))
}

// Healthy admission observes a published generation under a shared lease.
// Missing or incomplete bootstrap falls through to the exclusive transition,
// which rechecks all state before any start or identity publication.
func ProbePrivateAdmissionSupervisor(ctx context.Context, store *taskdir.Store, clientExecutable, daemonExecutable string, options pueue.Options) (*pueue.Client, error) {
	client, ready, err := pueue.ProbePrivateEndpointWithGate(ctx, clientExecutable, daemonExecutable, store.Root, options, func(checkContext context.Context, current *pueue.Client) (bool, error) {
		if err := rejectPrivateAdmissionMigrationAfterEndpointProbe(checkContext, store, current, true); err != nil {
			if errors.Is(err, ErrExternalInspectionHandoffRequired) {
				return false, nil
			}
			return false, err
		}
		observationReady, err := pueue.PrivateTaskObservationReady(store.Root)
		if err != nil || !observationReady {
			return false, err
		}
		return OrdinarySchedulingRecorded(store.Root, current.Binding())
	})
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, client.Close()
	}
	return client, nil
}

func privateSupervisorConfigExisted(root string) (bool, error) {
	_, err := os.Stat(pueue.PrivateConfigPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// rejectPrivateAdmissionMigrationAfterEndpointProbe reuses the liveness proof
// already established by the warm endpoint probe or a protocol-level group
// probe. It avoids an identical full status observation solely to decide
// whether a published live endpoint may remain in place.
func rejectPrivateAdmissionMigrationAfterEndpointProbe(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted bool) error {
	return RejectPrivateAdmissionMigrationWithEndpointProof(ctx, store, supervisor, configExisted, true)
}

func RejectPrivateAdmissionMigrationWithEndpointProof(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted, endpointProvenLive bool) error {
	if store == nil || supervisor == nil {
		return errors.New("missing private admission supervisor")
	}
	resolution, err := ResolveGenerationWithLock(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	if resolution.PrivateAuthority {
		if resolution.Active == supervisor.Binding() {
			return nil
		}
		return fmt.Errorf("%w: admission supervisor is not the active private generation", inspection.ErrGroupMigrationRequired)
	}
	if !resolution.GroupPresent {
		if !configExisted {
			return nil
		}
		bootstrapPending, bootstrapErr := pueue.PrivateBootstrapPending(store.Root)
		if bootstrapErr != nil {
			return bootstrapErr
		}
		if bootstrapPending {
			// A Delegate-owned bootstrap marker proves that this config was
			// created by a startup interrupted before identity publication.
			// It is safe to retry that same bootstrap; an unmarked config-only
			// root remains an unverified predecessor and fails closed below.
			return nil
		}
		if endpointProvenLive {
			return nil
		}
		return supervisor.VerifyPrivateMigrationEndpoint(ctx, store.Root)
	}
	if task.SameSupervisorIdentity(resolution.Active, supervisor.Binding()) {
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, resolution.GroupHead.Supervisor.ConfigPath) {
		return fmt.Errorf("%w: Delegate does not migrate an existing external supervisor", inspection.ErrGroupMigrationUnsupported)
	}
	return fmt.Errorf("%w: existing private inspection supervisor requires explicit migration", inspection.ErrGroupMigrationRequired)
}
