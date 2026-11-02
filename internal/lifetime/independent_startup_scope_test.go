//go:build linux || darwin

package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

func runIndependentScopeOwner(ctx context.Context, scopeRole, workerRole string) (resultErr error) {
	plan, err := Prepare("independent-component", scopeRole, strconv.Itoa(os.Getpid()))
	if err != nil {
		return err
	}
	if string(plan.Kind()) != os.Getenv("DELEGATE_INDEPENDENT_EXPECTED_BACKEND") {
		return fmt.Errorf("native backend mismatch: observed %s/%s", plan.Kind(), plan.Assurance())
	}
	scope, err := CreatePreparedContext(ctx, plan)
	if err != nil {
		return err
	}
	var command *exec.Cmd
	var launches []LaunchRef
	defer func() {
		resultErr = errors.Join(resultErr, closeIndependentScope(ctx, scope, command, launches, scopeRole))
	}()
	if err = scope.BindOwnerProcess(); err != nil {
		return err
	}
	command, err = independentFixtureCommand(workerRole)
	if err != nil {
		return err
	}
	if err = scope.Configure(command); err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	launch, err := scope.ObserveStarted(command.Process.Pid)
	if err != nil {
		return err
	}
	launches = []LaunchRef{launch}
	ref, err := ObserveProcess(os.Getpid())
	if err != nil {
		return err
	}
	leasePresent, err := independentScopeLease(scopeRole)
	if err != nil {
		return err
	}
	data, err := json.Marshal(independentFixtureRecord{PID: ref.PID(), Token: ref.StartToken(), Parent: os.Getppid(), LeasePresent: leasePresent, Backend: plan.Kind(), Assurance: plan.Assurance()})
	if err != nil {
		return err
	}
	if err = independentFixturePublish(scopeRole+".json", data); err != nil {
		return err
	}
	return independentFixtureWait(ctx, scopeRole+".release")
}

func closeIndependentScope(parent context.Context, scope *Scope, command *exec.Cmd, launches []LaunchRef, scopeRole string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	var waitErr error
	if command != nil && command.Process != nil {
		if err := scope.Terminate(ctx, launches); err != nil {
			return err
		}
		waitErr = command.Wait()
		var exited *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exited) {
			return waitErr
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
	return independentFixturePublish(scopeRole+".closed", []byte("empty-and-released\n"))
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
	_, err := os.Stat(filepath.Join(os.Getenv("DELEGATE_INDEPENDENT_FIXTURE_DIRECTORY"), "fixture.abort"))
	if err == nil {
		return context.Canceled
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// The daemon-like fixture retains the exact Wait of its B scope owner even
// when the resident above it exits. Its own release settles this child first.
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
	path := independentFixturePath("b-scope.release")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err = independentFixturePublish("b-scope.release", []byte("release\n")); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		_, err = os.Stat(independentFixturePath("b-scope.closed"))
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func independentScopeLease(role string) (bool, error) {
	if role == "b-scope" {
		return independentFixtureHasLease()
	}
	return false, nil
}
