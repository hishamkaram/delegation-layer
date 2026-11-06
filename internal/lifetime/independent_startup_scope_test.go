//go:build linux || darwin

package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"
)

type independentScopeDescriptor struct {
	Kind      Kind      `json:"kind"`
	Assurance Assurance `json:"assurance"`
	Plan      string    `json:"plan"`
	Scope     string    `json:"scope"`
}

func runIndependentScopeOwner(ctx context.Context, scopeRole, workerRole string) (resultErr error) {
	plan, err := Prepare("independent-component", scopeRole, strconv.Itoa(os.Getpid()))
	if err != nil {
		return err
	}
	if string(plan.Kind()) != os.Getenv("DELEGATE_INDEPENDENT_EXPECTED_BACKEND") {
		return errors.New("native backend does not match the declared proof context")
	}
	scope, err := CreatePreparedContext(ctx, plan)
	if err != nil {
		return err
	}
	var workerWait <-chan error
	var launches []LaunchRef
	defer func() {
		resultErr = errors.Join(resultErr, closeIndependentScope(ctx, scope, workerWait, launches, scopeRole, workerRole))
	}()
	if err = scope.BindOwnerProcess(); err != nil {
		return err
	}
	if err = publishIndependentScopeDescriptor(scopeRole, scope, plan); err != nil {
		return err
	}
	workerWait, err = startIndependentManagedWorker(scope, scopeRole, workerRole)
	if err != nil {
		return err
	}
	if err = independentFixtureWait(ctx, scopeRole+".provider-launch.json"); err != nil {
		return err
	}
	launch, err := observeIndependentProvider(scope, scopeRole)
	if err != nil {
		return err
	}
	launches = []LaunchRef{launch}
	if err = independentFixturePublish(scopeRole+".go", []byte("launch-observed\n")); err != nil {
		return err
	}
	if err = publishIndependentScopeIdentity(scopeRole, plan); err != nil {
		return err
	}
	return independentFixtureWait(ctx, scopeRole+".release")
}

func publishIndependentScopeDescriptor(role string, scope *Scope, plan Plan) error {
	descriptor := independentScopeDescriptor{Kind: plan.Kind(), Assurance: plan.Assurance(), Plan: plan.Identifier(), Scope: scope.Reference().Identifier()}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	return independentFixturePublish(role+".prepared.json", data)
}

func startIndependentManagedWorker(scope *Scope, scopeRole, workerRole string) (<-chan error, error) {
	command, err := independentFixtureCommand(workerRole)
	if err != nil {
		return nil, err
	}
	// As in guardian.startAndRecordWorker, the managed worker is infrastructure
	// outside the provider scope. It owns provider Start/Wait through a borrowed
	// registered scope; only this helper owns native termination and disposal.
	if err = command.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ref, err := ObserveStartedProcess(command.Process.Pid)
	if err != nil {
		return done, err
	}
	if err = scope.TrackOwnerProcess(ref); err != nil {
		return done, err
	}
	return done, independentFixturePublish(scopeRole+".worker-go", []byte("worker-tracked\n"))
}

func observeIndependentProvider(scope *Scope, role string) (LaunchRef, error) {
	data, err := os.ReadFile(independentFixturePath(role + ".provider-launch.json"))
	if err != nil {
		return LaunchRef{}, err
	}
	var record independentFixtureRecord
	if err = json.Unmarshal(data, &record); err != nil {
		return LaunchRef{}, err
	}
	launch, err := scope.ObserveStarted(record.PID)
	if err != nil {
		return launch, err
	}
	if !nestedFixtureTokensMatch(launch.startToken, record.Token) {
		return LaunchRef{}, errors.New("provider fixture launch identity changed")
	}
	return launch, nil
}

func publishIndependentScopeIdentity(role string, plan Plan) error {
	ref, err := ObserveProcess(os.Getpid())
	if err != nil {
		return err
	}
	leasePresent, err := independentScopeLease(role)
	if err != nil {
		return err
	}
	data, err := json.Marshal(independentFixtureRecord{PID: ref.PID(), Token: ref.StartToken(), Parent: os.Getppid(), LeasePresent: leasePresent, Backend: plan.Kind(), Assurance: plan.Assurance()})
	if err != nil {
		return err
	}
	return independentFixturePublish(role+".json", data)
}

func closeIndependentScope(parent context.Context, scope *Scope, workerWait <-chan error, launches []LaunchRef, scopeRole, workerRole string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	terminateErr := scope.Terminate(ctx, launches)
	releaseErr := releaseIndependentWorker(scopeRole, workerRole)
	waitErr := awaitIndependentWorker(ctx, workerWait)
	if err := errors.Join(terminateErr, releaseErr, waitErr); err != nil {
		return err
	}
	if len(launches) > 0 {
		if err := verifyIndependentProviderWait(scopeRole, launches[0]); err != nil {
			return err
		}
	}
	empty, err := scope.EmptyContext(ctx, launches)
	if err != nil || !empty {
		return errors.Join(errors.New("component task scope not proven empty"), err)
	}
	if err = scope.DestroyEmpty(); err != nil {
		return err
	}
	if err = scope.Release(); err != nil {
		return err
	}
	return independentFixturePublish(scopeRole+".closed", []byte("worker-wait-empty-released\n"))
}

func releaseIndependentWorker(scopeRole, workerRole string) error {
	// Before setup acknowledgement a fixed fixture may still be waiting for its
	// gate. Its own release is cleanup I/O, not scope-termination evidence.
	providerRole := "a-provider"
	if scopeRole == "b-scope" {
		providerRole = "b-provider"
	}
	return errors.Join(publishIndependentRelease(providerRole), publishIndependentRelease(workerRole))
}

func awaitIndependentWorker(ctx context.Context, done <-chan error) error {
	if done == nil {
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func publishIndependentRelease(role string) error {
	err := independentFixturePublish(role+".release", []byte("release\n"))
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

func verifyIndependentProviderWait(role string, launch LaunchRef) error {
	data, err := os.ReadFile(independentFixturePath(role + ".provider-wait.json"))
	if err != nil {
		return err
	}
	var record independentFixtureRecord
	if err = json.Unmarshal(data, &record); err != nil {
		return err
	}
	if !record.Waited || record.PID != launch.pid || !nestedFixtureTokensMatch(record.Token, launch.startToken) {
		return errors.New("provider fixture exact Wait not established")
	}
	return nil
}

// The adopted daemon retains its B scope helper's exact child Wait.
func runIndependentDaemon(ctx context.Context) (resultErr error) {
	command, err := independentFixtureCommand("b-scope-owner")
	if err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		resultErr = errors.Join(resultErr, closeIndependentDaemonScope(ctx, done))
		if resultErr == nil {
			resultErr = independentFixturePublish("daemon.closed", []byte("child-wait-and-scope-closure-observed\n"))
		}
	}()
	if err = independentFixtureWait(ctx, "b-scope.json"); err != nil {
		return err
	}
	if err = independentFixtureWait(ctx, "b-worker.json"); err != nil {
		return err
	}
	return runIndependentIdle(ctx, "daemon")
}

func closeIndependentDaemonScope(parent context.Context, done <-chan error) error {
	if err := publishIndependentRelease("b-scope"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	if err := awaitIndependentWorker(ctx, done); err != nil {
		return err
	}
	_, err := os.Stat(independentFixturePath("b-scope.closed"))
	return err
}

func independentScopeLease(role string) (bool, error) {
	if role == "b-scope" {
		return independentFixtureHasLease()
	}
	return false, nil
}
