//go:build linux || darwin

package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// This fixture mirrors the production worker's borrowed scope and exact Wait.
// It never binds itself as guardian, signals a process or destroys the scope.
func runIndependentManagedWorker(ctx context.Context, scopeRole, workerRole, providerRole string) (resultErr error) {
	if err := awaitIndependentFixtureGate(ctx, scopeRole+".worker-go", workerRole); err != nil {
		return err
	}
	scope, err := openIndependentWorkerScope(ctx, scopeRole)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, scope.Release()) }()
	command, err := independentFixtureCommand(providerRole)
	if err != nil {
		return err
	}
	if err = scope.Configure(command); err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ref, observeErr := ObserveStartedProcess(command.Process.Pid)
	defer func() {
		resultErr = errors.Join(resultErr, closeIndependentProvider(ctx, scopeRole, providerRole, ref, done))
	}()
	if observeErr != nil {
		return observeErr
	}
	data, err := json.Marshal(independentFixtureRecord{PID: ref.PID(), Token: ref.StartToken()})
	if err != nil {
		return err
	}
	if err = independentFixturePublish(scopeRole+".provider-launch.json", data); err != nil {
		return err
	}
	return runIndependentIdle(ctx, workerRole)
}

func openIndependentWorkerScope(ctx context.Context, role string) (*Scope, error) {
	data, err := os.ReadFile(independentFixturePath(role + ".prepared.json"))
	if err != nil {
		return nil, err
	}
	var descriptor independentScopeDescriptor
	if err = json.Unmarshal(data, &descriptor); err != nil {
		return nil, err
	}
	plan, err := NewPlan(descriptor.Kind, descriptor.Assurance, descriptor.Plan)
	if err != nil {
		return nil, err
	}
	ref, err := NewRef(descriptor.Kind, descriptor.Assurance, descriptor.Scope)
	if err != nil {
		return nil, err
	}
	return OpenRegisteredContext(ctx, plan, ref)
}

func closeIndependentProvider(parent context.Context, scopeRole, providerRole string, ref ProcessRef, done <-chan error) error {
	if err := publishIndependentRelease(providerRole); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	waitErr := awaitIndependentWorker(ctx, done)
	var exited *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exited) {
		return waitErr
	}
	if err := validateProcessRef(ref); err != nil {
		return err
	}
	data, err := json.Marshal(independentFixtureRecord{PID: ref.PID(), Token: ref.StartToken(), Waited: true})
	if err != nil {
		return err
	}
	return independentFixturePublish(scopeRole+".provider-wait.json", data)
}

func awaitIndependentFixtureGate(ctx context.Context, gate, role string) error {
	return waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		if _, err := os.Stat(independentFixturePath(role + ".release")); err == nil {
			return false, context.Canceled
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err := independentFixtureAbort(); err != nil {
			return false, err
		}
		_, err := os.Stat(independentFixturePath(gate))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}, sleepContext)
}

func runIndependentProvider(ctx context.Context, scopeRole, role string) error {
	if err := awaitIndependentFixtureGate(ctx, scopeRole+".go", role); err != nil {
		return err
	}
	if role == "a-provider" {
		if err := independentRequesterOrphan(); err != nil {
			return err
		}
		if err := independentFixturePublish("start", []byte("start\n")); err != nil {
			return err
		}
	}
	return runIndependentIdle(ctx, role)
}

func independentRequesterOrphan() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "-test.run=^TestNestedSurvivalFixture$")
	command.Dir = os.Getenv("DELEGATE_INDEPENDENT_FIXTURE_DIRECTORY")
	command.Env = append(os.Environ(),
		"DELEGATE_NESTED_FIXTURE_MODE=orphan-parent",
		"DELEGATE_NESTED_FIXTURE_OUTPUT="+independentFixturePath("ordinary.json"),
		"DELEGATE_NESTED_FIXTURE_RELEASE="+independentFixturePath("ordinary.release"),
		"DELEGATE_NESTED_FIXTURE_DEADLINE_NS="+strconv.FormatInt(independentFixtureDeadline().UnixNano(), 10))
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err = command.Start(); err != nil {
		return err
	}
	return command.Wait()
}

func independentFixtureAbort() error {
	_, err := os.Stat(independentFixturePath("fixture.abort"))
	if err == nil {
		return context.Canceled
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
