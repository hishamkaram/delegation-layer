package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PreparePrivateAdmissionSupervisorContext(ctx context.Context, lookup installation.ExecutableLookup, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if store == nil {
		return nil, errors.New("missing inspection state store")
	}
	for {
		probe, err := probePrivateAdmissionWithLookup(ctx, lookup, store, options)
		if err != nil || probe.client != nil {
			return probe.client, err
		}
		supervisor, acquired, transitionErr := electPrivateAdmissionTransition(ctx, lookup, store, options)
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

func electPrivateAdmissionTransition(ctx context.Context, lookup installation.ExecutableLookup, store *taskdir.Store, options pueue.Options) (*pueue.Client, bool, error) {
	var supervisor *pueue.Client
	acquired, err := pueue.TryWithPrivateTransition(ctx, store.Root, func() error {
		var probeErr error
		probe, probeErr := probePrivateAdmissionWithLookup(ctx, lookup, store, options)
		supervisor = probe.client
		if probeErr != nil || supervisor != nil {
			return probeErr
		}
		return pueue.WithBootstrapLockContext(ctx, store.Root, func(lockedContext context.Context) error {
			var prepareErr error
			supervisor, prepareErr = PreparePrivateAdmissionTransition(lockedContext, lookup, store, options)
			return prepareErr
		})
	})
	return supervisor, acquired, err
}

func PreparePrivateAdmissionTransition(ctx context.Context, lookup installation.ExecutableLookup, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	configExisted, err := privateSupervisorConfigExisted(store.Root)
	if err != nil {
		return nil, err
	}
	if err = pueue.EnsurePrivateTaskObservationWithBootstrapLock(store.Root); err != nil {
		return nil, err
	}
	// Preserve the canonical recovery owner's classification of unreadable
	// publication, witness and transition records before any live refresh.
	if _, err = pueue.ObservePublishedPrivatePairWithLock(ctx, store.Root, options); err != nil {
		return nil, err
	}
	// A live transport refresh owns no generation replacement. Completed history
	// can forbid replacing this binding without forbidding runtime capability evidence.
	clientExecutable, daemonExecutable, _, err := privateAdmissionTransport(ctx, lookup, store.Root)
	if err != nil {
		return nil, err
	}
	supervisor, ready, err := repairLivePrivateAdmissionTransition(ctx, store, options, configExisted, clientExecutable, daemonExecutable)
	if err != nil || ready {
		return supervisor, err
	}
	if err = ReconcilePublishedSupervisorWithLock(ctx, store, options, lookup, false); err != nil {
		return nil, err
	}
	supervisor, ready, err = repairLivePrivateAdmissionTransition(ctx, store, options, configExisted, clientExecutable, daemonExecutable)
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
	return bootstrapPrivateAdmission(ctx, lookup, store, options, configExisted)
}

func repairLivePrivateAdmissionTransition(ctx context.Context, store *taskdir.Store, options pueue.Options, configExisted bool, clientExecutable, daemonExecutable string) (*pueue.Client, bool, error) {
	supervisor, groups, ready, err := pueue.ProbePrivateGroupsWithBootstrapLock(ctx, clientExecutable, daemonExecutable, store.Root, options)
	if err != nil || !ready {
		return supervisor, ready, err
	}
	if err = resolvePrivateAdmissionMigrationWithBootstrapLock(ctx, store, supervisor, configExisted, true); err != nil {
		return supervisor, true, err
	}
	configured, err := OrdinarySchedulingConfiguredGroups(store.Root, supervisor.RuntimeBinding(), groups)
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

func bootstrapPrivateAdmission(ctx context.Context, lookup installation.ExecutableLookup, store *taskdir.Store, options pueue.Options, configExisted bool) (*pueue.Client, error) {
	clientExecutable, daemonExecutable, err := installation.ResolvePrivateSupervisorExecutables(lookup)
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
// AdmissionReadiness is a passive projection; only the resident may repair a miss.
type AdmissionReadiness uint8

const (
	AdmissionNotReady AdmissionReadiness = iota
	AdmissionReady
	AdmissionSchedulingRefreshRequired
)

type admissionProbe struct {
	client    *pueue.Client
	readiness AdmissionReadiness
}

func ProbePrivateAdmissionSupervisor(ctx context.Context, store *taskdir.Store, clientExecutable, daemonExecutable string, options pueue.Options) (*pueue.Client, error) {
	probe, err := probePrivateAdmissionSupervisor(ctx, store, clientExecutable, daemonExecutable, options)
	return probe.client, err
}

func probePrivateAdmissionSupervisor(ctx context.Context, store *taskdir.Store, clientExecutable, daemonExecutable string, options pueue.Options) (admissionProbe, error) {
	state := AdmissionNotReady
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
		recorded, err := OrdinarySchedulingRecorded(store.Root, current.RuntimeBinding())
		if err == nil && !recorded {
			state = AdmissionSchedulingRefreshRequired
		}
		return recorded, err
	})
	if err != nil {
		return admissionProbe{}, err
	}
	if !ready {
		return admissionProbe{readiness: state}, client.Close()
	}
	return admissionProbe{client: client, readiness: AdmissionReady}, nil
}

// Preliminary selection never supplies authority. Canonical probes retain faults
// from malformed records or unavailable selections and revalidate under their lease.
func privateAdmissionTransport(ctx context.Context, lookup installation.ExecutableLookup, root string) (string, string, bool, error) {
	published, found, readErr := pueue.ReadPrivateSupervisorBinding(root)
	needsPair := false
	if readErr == nil && found {
		missing, pairErr := pueue.PrivateSupervisorPairMissing(published)
		needsPair = pairErr == nil && (missing || pueue.IsLegacyStateSupervisorClient(root, published))
	}
	if !needsPair {
		// Record and metadata faults belong to the canonical probe, which
		// retains their exact wrapped cause instead of replacing it with lookup.
		return "", "", false, nil
	}
	client, daemon, err := resolveCompatiblePrivatePair(ctx, lookup)
	if err != nil && task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, pueue.ErrCompatiblePairUnavailable) }) {
		return "", "", false, nil
	}
	return client, daemon, err == nil, err
}

func probePrivateAdmissionWithLookup(ctx context.Context, lookup installation.ExecutableLookup, store *taskdir.Store, options pueue.Options) (admissionProbe, error) {
	client, daemon, selected, err := privateAdmissionTransport(ctx, lookup, store.Root)
	if err != nil {
		return admissionProbe{}, err
	}
	if selected {
		probe, err := probePrivateAdmissionSupervisor(ctx, store, client, daemon, options)
		if err != nil || probe.readiness != AdmissionNotReady {
			return probe, err
		}
	}
	return probePrivateAdmissionSupervisor(ctx, store, "", "", options)
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
