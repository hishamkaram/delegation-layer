package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ErrExecutionTargetUnverified marks every observation that could not prove
// which process would run newly admitted work. It is advisory: the caller
// decides how an unknown target affects its own policy.
var ErrExecutionTargetUnverified = errors.New("supervisor execution target is unverified")

// ExecutionTargetState is the read-only fact observed for the supervisor
// process that would run newly admitted work. The zero value is no fact.
type ExecutionTargetState uint8

const (
	// ExecutionTargetUnknown is no verified fact.
	ExecutionTargetUnknown ExecutionTargetState = iota
	// ExecutionTargetAbsent means no private daemon was recorded or reachable
	// at observation: the root has no private supervisor directory, or its
	// complete generation has neither a PID record nor an endpoint socket.
	// Admission would start the daemon from the calling process.
	ExecutionTargetAbsent
	// ExecutionTargetRunning is a daemon whose recorded PID, native start
	// token, and connected endpoint peer credentials agree.
	ExecutionTargetRunning
)

// ExecutionTarget is one immutable observation of the private supervisor
// process. Only this package creates verified values.
type ExecutionTarget struct {
	state      ExecutionTargetState
	pid        int
	startToken string
}

// State returns the observed fact.
func (t ExecutionTarget) State() ExecutionTargetState { return t.state }

// PID returns the verified daemon PID, or zero when the target is not running.
func (t ExecutionTarget) PID() int { return t.pid }

// StartToken returns the native process start token bound to PID, or empty
// when the target is not running.
func (t ExecutionTarget) StartToken() string { return t.startToken }

// privateEndpointPeer is the credential the kernel reports for the process
// that accepted a connection on the private endpoint.
type privateEndpointPeer struct {
	PID int
	UID int
}

// ObservePrivateExecutionTarget reports the private supervisor process for a
// Delegate state root without locks, writes, subprocesses, signals, repair, or
// start. Every failure, unsupported platform, transition, or identity race is
// returned as an error wrapping ErrExecutionTargetUnverified with an unknown
// target.
func ObservePrivateExecutionTarget(ctx context.Context, stateRoot string, options Options) (ExecutionTarget, error) {
	target, err := observePrivateExecutionTarget(ctx, stateRoot, options)
	if err != nil {
		return ExecutionTarget{}, errors.Join(ErrExecutionTargetUnverified, err)
	}
	return target, nil
}

func observePrivateExecutionTarget(ctx context.Context, stateRoot string, options Options) (target ExecutionTarget, resultErr error) {
	if ctx == nil {
		return ExecutionTarget{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return ExecutionTarget{}, err
	}
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ExecutionTarget{}, ErrConfiguration
	}
	_, err := os.Lstat(filepath.Join(stateRoot, privateSupervisorDirectory))
	if errors.Is(err, os.ErrNotExist) {
		return ExecutionTarget{state: ExecutionTargetAbsent}, nil
	}
	if err != nil {
		return ExecutionTarget{}, fmt.Errorf("%w: inspect private supervisor directory: %w", ErrBinding, err)
	}
	published, err := completePrivateExecutionGeneration(stateRoot)
	if err != nil {
		return ExecutionTarget{}, err
	}
	observer, err := NewClient(published, options)
	if err != nil {
		return ExecutionTarget{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, observer.Close()) }()
	target, err = observer.observeExecutionTarget(ctx)
	if err != nil {
		return ExecutionTarget{}, err
	}
	current, err := completePrivateExecutionGeneration(stateRoot)
	if err != nil {
		return ExecutionTarget{}, err
	}
	if current != published {
		return ExecutionTarget{}, fmt.Errorf("%w: private supervisor generation changed during observation", ErrBinding)
	}
	return target, nil
}

// completePrivateExecutionGeneration returns the published binding only when
// the canonical generation decision is complete: no bootstrap, publication,
// migration, or recovery alias is pending.
func completePrivateExecutionGeneration(stateRoot string) (task.SupervisorRef, error) {
	facts, err := loadPrivateGenerationFacts(stateRoot)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if facts.publishedErr != nil {
		return task.SupervisorRef{}, facts.publishedErr
	}
	if !facts.publishedFound {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor generation is not published", ErrBinding)
	}
	phase, err := facts.phase(facts.published)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if phase != privateGenerationComplete {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor generation transition is pending", ErrBinding)
	}
	return facts.published, nil
}

func (c *Client) observeExecutionTarget(ctx context.Context) (ExecutionTarget, error) {
	authenticated, err := c.runtimeConfigAuthenticated()
	if err != nil {
		return ExecutionTarget{}, err
	}
	if !authenticated {
		return ExecutionTarget{}, fmt.Errorf("%w: private supervisor config changed after publication", ErrBinding)
	}
	path, err := privateEndpointSocketPath(c.runtimeBinding.Endpoint)
	if err != nil {
		return ExecutionTarget{}, err
	}
	if err = rejectSymlinkComponents(path); err != nil {
		return ExecutionTarget{}, err
	}
	pid, recorded, err := c.privateDaemonPID()
	if err != nil {
		return ExecutionTarget{}, err
	}
	if !recorded {
		return stoppedPrivateExecutionTarget(path)
	}
	before, _, err := c.privateProcessIdentity(pid)
	if err != nil {
		return ExecutionTarget{}, err
	}
	peerContext, cancel := context.WithTimeout(ctx, c.options.ObservationTimeout)
	peer, err := observeUnixEndpointPeer(peerContext, path)
	cancel()
	if err != nil {
		return ExecutionTarget{}, err
	}
	after, _, err := c.privateProcessIdentity(pid)
	if err != nil {
		return ExecutionTarget{}, err
	}
	pidAfter, recordedAfter, err := c.privateDaemonPID()
	if err != nil {
		return ExecutionTarget{}, err
	}
	if !recordedAfter || pidAfter != pid {
		return ExecutionTarget{}, fmt.Errorf("%w: private supervisor PID record changed during observation", ErrBinding)
	}
	if err = verifyPrivateExecutionPeer(before, after, peer, os.Geteuid()); err != nil {
		return ExecutionTarget{}, err
	}
	return ExecutionTarget{state: ExecutionTargetRunning, pid: pid, startToken: before.StartToken}, nil
}

// stoppedPrivateExecutionTarget classifies a complete generation without a
// PID record. Only a missing endpoint is absence; an existing socket inode,
// refused or not, has an unproven owner.
func stoppedPrivateExecutionTarget(path string) (ExecutionTarget, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ExecutionTarget{state: ExecutionTargetAbsent}, nil
	}
	if err != nil {
		return ExecutionTarget{}, fmt.Errorf("%w: inspect private supervisor endpoint: %w", ErrBinding, err)
	}
	return ExecutionTarget{}, fmt.Errorf("%w: private supervisor endpoint exists without a recorded PID", ErrBinding)
}

// verifyPrivateExecutionPeer binds the endpoint peer to one process
// incarnation: the recorded PID kept the same nonempty start token on both
// sides of the connection, and the kernel reported that PID and the caller's
// effective UID for the accepting process.
func verifyPrivateExecutionPeer(before, after privateProcessIdentity, peer privateEndpointPeer, uid int) error {
	if before.StartToken == "" || after.StartToken == "" {
		return fmt.Errorf("%w: recorded private supervisor process is absent", ErrBinding)
	}
	if before.PID != after.PID || before.StartToken != after.StartToken {
		return fmt.Errorf("%w: private supervisor process identity changed during observation", ErrBinding)
	}
	if peer.PID != before.PID {
		return fmt.Errorf("%w: private supervisor endpoint peer is not the recorded process", ErrBinding)
	}
	if peer.UID != uid {
		return fmt.Errorf("%w: private supervisor endpoint peer belongs to another user", ErrBinding)
	}
	return nil
}
