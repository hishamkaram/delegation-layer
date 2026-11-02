//go:build linux || darwin

package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const independentFixtureMode = "DELEGATE_INDEPENDENT_FIXTURE_MODE"

type independentFixtureRecord struct {
	PID          int       `json:"pid"`
	Token        string    `json:"token"`
	Parent       int       `json:"parent"`
	LeasePresent bool      `json:"lease_present"`
	Backend      Kind      `json:"backend,omitempty"`
	Assurance    Assurance `json:"assurance,omitempty"`
}

func TestIndependentStartupFixture(t *testing.T) {
	mode := os.Getenv(independentFixtureMode)
	if mode == "" {
		t.Skip("fixed native startup component fixture")
	}
	ctx, cancel := context.WithDeadline(t.Context(), independentFixtureDeadline())
	defer cancel()
	if err := runIndependentStartupFixture(ctx, mode); err != nil {
		t.Fatal(err)
	}
}

func independentFixtureDeadline() time.Time {
	value, err := strconv.ParseInt(os.Getenv("DELEGATE_INDEPENDENT_FIXTURE_DEADLINE"), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, value)
}

func independentFixturePath(name string) string {
	return filepath.Join(os.Getenv("DELEGATE_INDEPENDENT_FIXTURE_DIRECTORY"), name)
}

func runIndependentStartupFixture(ctx context.Context, mode string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch mode {
	case "origin":
		return runIndependentOrigin(ctx)
	case "resident":
		return runIndependentResident(ctx)
	case "daemon":
		return runIndependentDaemon(ctx)
	case "b-worker":
		return runIndependentIdle(ctx, "b-worker")
	case "b-scope-owner":
		return runIndependentScopeOwner(ctx, "b-scope", "b-worker")
	case "requester":
		if err := independentRequesterOrphan(); err != nil {
			return err
		}
		if err := independentFixturePublish("start", []byte("start\n")); err != nil {
			return err
		}
		return runIndependentIdle(ctx, "requester")
	case "scope-owner":
		return runIndependentScopeOwner(ctx, "scope", "requester")
	default:
		return errors.New("unknown independent startup fixture mode")
	}
}

func independentFixtureCommand(mode string) (*exec.Cmd, error) {
	if err := independentFixtureAbort(); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, "-test.run=^TestIndependentStartupFixture$", "-test.timeout=2m")
	command.Dir = os.Getenv("DELEGATE_INDEPENDENT_FIXTURE_DIRECTORY")
	command.Env = append(os.Environ(), independentFixtureMode+"="+mode)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return command, nil
}

func independentFixturePublish(name string, data []byte) error {
	return publishNativeFixtureRecord(independentFixturePath(name), false, func(w io.Writer) error {
		n, err := w.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		return err
	})
}

func independentFixtureObserve(role string, leasePresent bool) error {
	ref, err := ObserveProcess(os.Getpid())
	if err != nil {
		return err
	}
	data, err := json.Marshal(independentFixtureRecord{PID: ref.PID(), Token: ref.StartToken(), Parent: os.Getppid(), LeasePresent: leasePresent})
	if err != nil {
		return err
	}
	return independentFixturePublish(role+".json", data)
}

func independentFixtureWait(ctx context.Context, name string) error {
	return waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		if name == "start" {
			if err := independentFixtureAbort(); err != nil {
				return false, err
			}
		}
		_, err := os.Stat(independentFixturePath(name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}, sleepContext)
}

func runIndependentOrigin(ctx context.Context) (resultErr error) {
	if err := independentFixtureObserve("origin", false); err != nil {
		return err
	}
	if err := independentFixtureWait(ctx, "start"); err != nil {
		return err
	}
	lease, err := os.OpenFile(independentFixturePath("instance.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	if err = unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	command, err := independentFixtureCommand("resident")
	if err != nil {
		return err
	}
	command.ExtraFiles = []*os.File{lease}
	if err = ConfigureIndependentSession(command); err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	// Resident infrastructure owns its lifetime after handshake. Keep the actual
	// child Wait while this parent lives; process exit can transfer only adoption.
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if err = independentFixtureWait(ctx, "resident.json"); err != nil {
		return err
	}
	if err = independentFixturePublish("origin-handoff", []byte("assumed\n")); err != nil {
		return err
	}
	if err = independentFixtureWait(ctx, "origin.release"); err != nil {
		return err
	}
	select {
	case err = <-done:
		return errors.Join(errors.New("resident exited before origin release"), err)
	default:
		return nil
	}
}

func independentFixtureLease() (*os.File, error) {
	const inheritedFD = 3
	var inherited, named unix.Stat_t
	if err := unix.Fstat(inheritedFD, &inherited); err != nil {
		return nil, err
	}
	if err := unix.Stat(independentFixturePath("instance.lock"), &named); err != nil {
		return nil, err
	}
	if inherited.Dev != named.Dev || inherited.Ino != named.Ino || inherited.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("inherited instance identity mismatch")
	}
	if _, err := unix.FcntlInt(inheritedFD, unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		return nil, err
	}
	lease := os.NewFile(inheritedFD, "independent-fixture-instance")
	if lease == nil {
		return nil, errors.New("invalid inherited instance descriptor")
	}
	return lease, nil
}

func runIndependentResident(ctx context.Context) (resultErr error) {
	lease, err := independentFixtureLease()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	present, err := independentFixtureHasLease()
	if err != nil || !present {
		return errors.Join(errors.New("resident must hold inherited instance lease"), err)
	}
	command, err := independentFixtureCommand("daemon")
	if err != nil {
		return err
	}
	if err = ConfigureIndependentSession(command); err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if err = independentFixtureWait(ctx, "daemon.json"); err != nil {
		return err
	}
	if err = independentFixtureObserve("resident", present); err != nil {
		return err
	}
	if err = independentFixtureWait(ctx, "resident.release"); err != nil {
		return err
	}
	// Abrupt exit models a lost resident while the finite owned child remains.
	// The test's independent release/backstop owns that adopted fixture's closure.
	if os.Getenv("DELEGATE_INDEPENDENT_FIXTURE_ABRUPT") == "1" {
		os.Exit(23)
	}
	select {
	case err = <-done:
		return errors.Join(errors.New("daemon exited before resident release"), err)
	default:
		return nil
	}
}

func independentFixtureHasLease() (bool, error) {
	info, err := os.Stat(independentFixturePath("instance.lock"))
	if err != nil {
		return false, err
	}
	names, err := os.ReadDir(independentFixtureFDDirectory())
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if _, err = strconv.Atoi(name.Name()); err != nil {
			continue
		}
		current, statErr := os.Stat(filepath.Join(independentFixtureFDDirectory(), name.Name()))
		if errors.Is(statErr, os.ErrNotExist) || errors.Is(statErr, unix.EBADF) {
			continue
		}
		if statErr != nil {
			return false, statErr
		}
		if os.SameFile(info, current) {
			return true, nil
		}
	}
	return false, nil
}

func runIndependentIdle(ctx context.Context, role string) error {
	present := false
	if role == "daemon" || role == "b-worker" {
		var err error
		present, err = independentFixtureHasLease()
		if err != nil {
			return err
		}
	}
	if err := independentFixtureObserve(role, present); err != nil {
		return err
	}
	for sequence := 1; ; sequence++ {
		name := fmt.Sprintf("%s.challenge.%d", role, sequence)
		readyCtx, cancel := context.WithCancel(ctx)
		err := waitForEmpty(readyCtx, 10*time.Millisecond, func() (bool, error) {
			if _, releaseErr := os.Stat(independentFixturePath(role + ".release")); releaseErr == nil {
				return true, nil
			} else if !errors.Is(releaseErr, os.ErrNotExist) {
				return false, releaseErr
			}
			_, challengeErr := os.Stat(independentFixturePath(name))
			if errors.Is(challengeErr, os.ErrNotExist) {
				return false, nil
			}
			return challengeErr == nil, challengeErr
		}, sleepContext)
		cancel()
		if err != nil {
			return err
		}
		if _, err = os.Stat(independentFixturePath(role + ".release")); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = independentFixturePublish(name+".reply", []byte("alive\n")); err != nil {
			return err
		}
	}
}
