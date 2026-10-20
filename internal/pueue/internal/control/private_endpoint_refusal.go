package control

import (
	"context"
	"errors"
	"fmt"
)

// ErrPrivateEndpointRefused identifies the observer error projected from a
// PrivateEndpointRefused observation. It is never proof of absence, grants no
// cleanup or start authority, and is always joined with ErrBinding.
var ErrPrivateEndpointRefused = errors.New("private supervisor endpoint refused connections")

const (
	privateEndpointOwnerUnknownReason = "endpoint_owner_unknown"
	privateEndpointRefusedLiveReason  = "endpoint_refused_live"
)

// PrivateEndpointState is the readiness fact observed at the exact private
// endpoint. The zero value is no fact: the endpoint was not proven ready,
// absent, or refused.
type PrivateEndpointState uint8

const (
	// PrivateEndpointReady is a valid bounded readiness response.
	PrivateEndpointReady PrivateEndpointState = iota + 1
	// PrivateEndpointAbsent is a clean completed command whose exact Unix
	// socket does not exist.
	PrivateEndpointAbsent
	// PrivateEndpointRefused is a clean completed command whose exact Unix
	// socket refused connections. It is never proof of absence.
	PrivateEndpointRefused
)

// PrivateEndpointObservation is one readiness observation of the exact
// private endpoint, returned beside any operational error. It carries no
// cleanup or start authority; only the exclusive closure acts on a refusal.
type PrivateEndpointObservation struct {
	state PrivateEndpointState
	cause error
}

// State returns the observed fact.
func (o PrivateEndpointObservation) State() PrivateEndpointState { return o.state }

// Cause returns the diagnostic command error behind an absent or refused
// endpoint, or nil.
func (o PrivateEndpointObservation) Cause() error { return o.cause }

func (o PrivateEndpointObservation) ready() bool { return o.state == PrivateEndpointReady }

// projectedError is the existing observer error for a refusal: it keeps
// ErrBinding, ErrPrivateEndpointRefused, and the command cause. Ready and
// absent observations project to no error.
func (o PrivateEndpointObservation) projectedError() error {
	if o.state != PrivateEndpointRefused {
		return nil
	}
	return errors.Join(ErrBinding, ErrPrivateEndpointRefused, o.cause)
}

// privateRefusalCompletion accepts only a command that started, completed
// naturally with a nonzero exit, and produced no response. Its error must be
// a whole-error ordinary completion; pending ownership, captured stdout, a
// control limit, or any independent fault keeps the original failure.
func privateRefusalCompletion(result CommandResult, err error) bool {
	if pendingFrom(err) != nil || !result.Started || len(result.Stdout) != 0 || result.ExitCode <= 0 || errors.Is(result.Err, ErrControlLimit) {
		return false
	}
	return classifyCommandCompletion(result.Err).kind == commandCompletionExit
}

// failPrivateTransitionClient keeps a carried refusal as the cause of a later
// failure, then releases the client.
func failPrivateTransitionClient(client *Client, observation PrivateEndpointObservation, err error) error {
	// The closure's own failures already wrap the refusal; join it only once.
	if refusal := observation.projectedError(); refusal != nil && !errors.Is(err, ErrPrivateEndpointRefused) {
		err = errors.Join(err, refusal)
	}
	return errors.Join(err, client.CloseAfterPending(err))
}

// availableForTransition observes the endpoint for a caller holding the
// exclusive bootstrap lock. cleanupSuccessful keeps each caller's existing
// status-overflow policy.
func (c *Client) availableForTransition(ctx context.Context, cleanupSuccessful bool) (PrivateEndpointObservation, error) {
	_, observation, err := privateClientObserveSnapshot(ctx, c, cleanupSuccessful)
	return observation, err
}

// closeRefusedPrivateEndpoint is the single closure for a refused private
// endpoint. The caller holds WithBootstrapLockContext for this state root and
// has already applied its generation, provenance, journal, and binding checks.
// Any observation other than a refusal is a no-op. A missing PID or any present recorded process
// fails closed with a typed reason; only a proven-absent recorded daemon lets
// removeStalePrivateEndpoint remove the owned socket. It never signals.
func (c *Client) closeRefusedPrivateEndpoint(ctx context.Context, observation PrivateEndpointObservation) error {
	refusal := observation.projectedError()
	if refusal == nil {
		return nil
	}
	if !isPrivateClient(c) {
		return errors.Join(refusal, ErrConfiguration)
	}
	path, err := privateEndpointSocketPath(c.runtimeBinding.Endpoint)
	if err != nil {
		return errors.Join(refusal, err)
	}
	pid, recorded, err := c.privateDaemonPID()
	if err != nil {
		return errors.Join(refusal, err)
	}
	if !recorded {
		return &PrivateRecoveryError{ReasonCode: privateEndpointOwnerUnknownReason, Err: refusal}
	}
	process, present, err := c.privateProcessIdentity(pid)
	if err != nil {
		return errors.Join(refusal, err)
	}
	if present {
		return &PrivateRecoveryError{ReasonCode: privateEndpointRefusedLiveReason, Err: refusal}
	}
	unavailable, err := c.privateEndpointUnavailable(ctx, path, process, true, false)
	if err != nil {
		return errors.Join(refusal, err)
	}
	if !unavailable {
		return errors.Join(refusal, fmt.Errorf("%w: private supervisor endpoint changed during refused-endpoint closure", ErrBinding))
	}
	return nil
}
