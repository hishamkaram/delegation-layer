package control

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// CompatiblePairSelector selects the installed executable pair. It is
// called at most once, inside the same shared observation, only when the exact
// published pair cannot verify and canonical completion permits compatibility.
type CompatiblePairSelector func(context.Context) (clientExecutable, daemonExecutable string, err error)

// ErrCompatiblePairUnavailable is a selector's positive no-candidate fact.
// Independent lookup, context or cleanup faults must remain separate causes.
var ErrCompatiblePairUnavailable = errors.New("compatible supervisor pair unavailable")

// ProbeSavedPrivateCompatible preserves the exact observable probe first,
// including same-generation recovery aliases. This probe never starts, repairs,
// publishes, or mutates the queue. The caller owns a returned client; its later
// operations retain their canonical policies. Nil with nil error means not ready.
func ProbeSavedPrivateCompatible(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options, selectPair CompatiblePairSelector) (*Client, error) {
	return probeSavedPrivateForPurpose(ctx, stateRoot, saved, options, selectPair, privateGenerationGeneral)
}

// ProbeSavedPrivateTaskObservation may read an equivalent same-generation
// recovery alias. Its caller must defer maintenance for that runtime binding.
// This probe never starts a daemon or grants submission or cleanup authority.
func ProbeSavedPrivateTaskObservation(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options, selectPair CompatiblePairSelector) (*Client, error) {
	return probeSavedPrivateForPurpose(ctx, stateRoot, saved, options, selectPair, privateGenerationTaskRead)
}

func probeSavedPrivateForPurpose(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options, selectPair CompatiblePairSelector, purpose privateGenerationPurpose) (*Client, error) {
	probeContext, cancel, options, err := prepareSavedPrivateProbeOptions(ctx, stateRoot, saved, options)
	if err != nil {
		return nil, err
	}
	defer cancel()
	present, err := privateBootstrapLockPresent(stateRoot)
	if err != nil || !present {
		return nil, err
	}
	var client *Client
	var ready bool
	var observation PrivateEndpointObservation
	err = WithSupervisorLeaseContext(probeContext, stateRoot, func(locked context.Context) error {
		var probeErr error
		client, ready, observation, probeErr = probeSavedPrivateCompatibleWithLock(locked, stateRoot, saved, options, selectPair, purpose)
		return probeErr
	})
	if err != nil {
		return finishSavedPrivateProbe(saved, nil, false, failPrivateTransitionClient(client, observation, err))
	}
	return finishSavedPrivateProbe(saved, client, ready, nil)
}

func probeSavedPrivateCompatibleWithLock(ctx context.Context, root string, saved task.SupervisorRef, options Options, selectPair CompatiblePairSelector, purpose privateGenerationPurpose) (*Client, bool, PrivateEndpointObservation, error) {
	published, found, err := savedPrivateProbeBinding(root, saved, purpose)
	if err != nil || !found {
		return nil, false, PrivateEndpointObservation{}, err
	}
	_, resolution, err := privateProbeResolutions(published, options)
	if err != nil {
		return nil, false, PrivateEndpointObservation{}, err
	}
	resolved, err := readPrivateConfig(published.ConfigPath, filepath.Dir(published.ConfigPath), resolution)
	if err != nil {
		return nil, false, PrivateEndpointObservation{}, err
	}
	directories, err := privateProbeDirectoriesReady(resolved)
	if err != nil || !directories {
		return nil, false, PrivateEndpointObservation{}, err
	}
	// The published generation owns resolution, independently of the saved
	// predecessor or the future installation selected only on fallback.
	if published.ResolutionOS != "" {
		options.Resolution = nil
		options, err = optionsForSavedBinding(published, options)
		if err != nil {
			return nil, false, PrivateEndpointObservation{}, err
		}
	}
	if IsLegacyStateSupervisorClient(root, published) {
		client, ready, probeErr := probeCompatibleSavedPair(ctx, root, saved, published, options, selectPair)
		return client, ready, PrivateEndpointObservation{}, probeErr
	}
	exact, verification, observation, err := probeExactPrivatePair(ctx, published, resolution, options)
	if err != nil {
		return exact, false, observation, err
	}
	if verification == privateProbePairNotReady && purpose == privateGenerationTaskRead {
		if closeErr := exact.Close(); closeErr != nil {
			return nil, false, PrivateEndpointObservation{}, closeErr
		}
		return probeSavedPrivateRecoveryAlias(ctx, root, saved, published, options)
	}
	if verification != privateProbePairUnverified {
		ready, verifyErr := verifyExactSavedPrivateObservation(ctx, root, saved, exact, observation, purpose)
		return exact, ready, observation, verifyErr
	}
	client, ready, err := probeCompatibleSavedPair(ctx, root, saved, published, options, selectPair)
	return client, ready, PrivateEndpointObservation{}, err
}

// Only TaskRead's additional alias equivalence requires the same saved
// generation. General readiness keeps connected completed successors intact.
func savedPrivateProbeBinding(root string, saved task.SupervisorRef, purpose privateGenerationPurpose) (task.SupervisorRef, bool, error) {
	facts, err := loadPrivateGenerationFacts(root)
	if err != nil {
		return task.SupervisorRef{}, false, err
	}
	if purpose == privateGenerationTaskRead && facts.publishedFound && !task.SameSupervisorIdentity(saved, facts.published) {
		_, general, generalErr := facts.probeBinding(privateGenerationObservable, privateGenerationGeneral)
		if generalErr != nil || !general {
			return task.SupervisorRef{}, false, generalErr
		}
	}
	return facts.probeBinding(privateGenerationObservable, purpose)
}

// Exact snapshots require a connected canonical successor for both purposes.
// An unrelated complete generation stays not ready so its owner can be routed.
func verifyExactSavedPrivateObservation(ctx context.Context, root string, saved task.SupervisorRef, client *Client, observation PrivateEndpointObservation, purpose privateGenerationPurpose) (bool, error) {
	if !observation.ready() {
		return false, nil
	}
	current, connected, err := resolvePrivateSupervisorSuccessorWithLock(ctx, root, saved, purpose)
	return connected && current == client.Binding(), err
}

func probeSavedPrivateRecoveryAlias(ctx context.Context, root string, saved, published task.SupervisorRef, options Options) (*Client, bool, PrivateEndpointObservation, error) {
	if !task.SameSupervisorIdentity(saved, published) {
		return nil, false, PrivateEndpointObservation{}, nil
	}
	alias, found, err := readValidatedPrivateRecoveryAlias(root)
	if err != nil || !found {
		return nil, false, PrivateEndpointObservation{}, err
	}
	if !alias.matches(published) {
		return nil, false, PrivateEndpointObservation{}, nil
	}
	client, err := bindPrivateRecoveryTransport(ctx, alias.record, saved, published.ClientExecutable, published.DaemonExecutable, options)
	if err != nil {
		return nil, false, PrivateEndpointObservation{}, err
	}
	client, _, observation, err := probeVerifiedPrivateClient(ctx, client, privateProbeSnapshot)
	if err != nil || !observation.ready() {
		return client, false, observation, err
	}
	err = client.verifyPrivateTaskObservationWithLock(ctx, root)
	return client, err == nil, observation, err
}

func probeCompatibleSavedPair(ctx context.Context, root string, saved, published task.SupervisorRef, options Options, selectPair CompatiblePairSelector) (*Client, bool, error) {
	current, complete, err := completePrivateProbeBinding(root, privateGenerationComplete)
	if err != nil || !complete {
		return nil, false, err
	}
	if current != published {
		return nil, false, ErrBinding
	}
	mapped, connected, err := ResolvePrivateSupervisorSuccessorWithLock(ctx, root, saved)
	if err != nil || !connected {
		return nil, false, err
	}
	if mapped != published {
		return nil, false, ErrBinding
	}
	if selectPair == nil {
		return nil, false, ErrConfiguration
	}
	clientExecutable, daemonExecutable, err := selectPair(ctx)
	if err != nil {
		if task.AllErrorCauses(err, compatiblePairUnavailable) {
			return nil, false, nil
		}
		return nil, false, err
	}
	client, ready, err := probePrivateCompatiblePair(ctx, saved, published, clientExecutable, daemonExecutable, options)
	if err != nil || !ready {
		return client, ready, err
	}
	current, complete, err = completePrivateProbeBinding(root, privateGenerationComplete)
	if err != nil || !complete || current != published {
		return client, false, errors.Join(ErrBinding, err)
	}
	return client, true, ctx.Err()
}

func compatiblePairUnavailable(cause error) bool {
	return errors.Is(cause, ErrCompatiblePairUnavailable)
}
