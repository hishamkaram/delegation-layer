package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// Fixed negative roles use the real owner and an actual SH resource. Native
// signals and descriptor observations remain with the lifetime proof driver.
func TestAdmissionControlNegativeFixture(t *testing.T) {
	if os.Getenv("DELEGATE_ADMISSION_LEASE_NEGATIVE") != "1" {
		t.Skip("fixed native admission negative caller")
	}
	deadline, err := strconv.ParseInt(os.Getenv("DELEGATE_ADMISSION_LEASE_DEADLINE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, deadline))
	defer cancel()
	if err = runAdmissionNegativeFixture(ctx); err != nil {
		t.Fatal(err)
	}
}

func runAdmissionNegativeFixture(ctx context.Context) (resultErr error) {
	root := os.Getenv("DELEGATE_ADMISSION_LEASE_DIRECTORY")
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return ErrConfiguration
	}
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
	if err != nil {
		return err
	}
	lease, err := store.AcquireRootAdmissionContext(ctx)
	defer func() {
		releaseErr := lease.Release()
		resultErr = errors.Join(resultErr, releaseErr)
		if releaseErr == nil {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	if err != nil {
		return err
	}
	ctx = WithControlLease(ctx, lease)
	base := filepath.Join(root, privateSupervisorDirectory)
	files, err := qualifiedCallerLeases(base)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeControlLeaseFiles(files)) }()
	ctx = WithControlLease(ctx, qualifiedFixtureLease{files: files})
	return WithBootstrapLockContext(ctx, root, func(locked context.Context) error {
		return runAdmissionNegativeCommand(locked, root)
	})
}

func runAdmissionNegativeCommand(ctx context.Context, root string) (resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	base := filepath.Join(root, privateSupervisorDirectory)
	role := strings.TrimPrefix(os.Getenv("DELEGATE_ADMISSION_LEASE_ROLE"), "negative-")
	options := Options{ControlOwnerExecutable: os.Getenv("DELEGATE_ADMISSION_LEASE_OWNER"), ObservationTimeout: time.Second, WaitForPendingClose: true, Environment: []string{"HOME=" + root, "PATH=/usr/bin:/bin", "LANG=C"}}
	if strings.HasPrefix(role, "managed-repair") {
		deadline, bounded := ctx.Deadline()
		if !bounded {
			return ErrConfiguration
		}
		options.ObservationTimeout = time.Until(deadline)
		// This is a fixed, isolated fixture process. Deliberately exercise native
		// 0775/0664 creation and an unusable ambient temp parent on both hosts.
		unix.Umask(0o002)
		options.Environment = append(options.Environment, "TMPDIR=/unavailable/hostile-temp")
	}
	client, err := Bind(ctx, os.Getenv("DELEGATE_ADMISSION_LEASE_PUEUE"), filepath.Join(base, "operator.yml"), options)
	if err != nil {
		return errors.Join(err, client.CloseAfterPending(err))
	}
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	started := make(chan error, 1)
	client.options.Observer = admissionNegativeObserver(base, role, started)
	completed := make(chan error, 1)
	go func() { completed <- admissionNegativeCommand(ctx, client, role, base) }()
	select {
	case err = <-started:
		if err != nil {
			cancel()
			return errors.Join(err, <-completed)
		}
	case err = <-completed:
		return errors.Join(errors.New("negative actor completed before Start observation"), err)
	case <-ctx.Done():
		return errors.Join(ctx.Err(), <-completed)
	}
	if err = waitQualifiedCallerRelease(ctx, base); err != nil {
		cancel()
		return errors.Join(err, <-completed)
	}
	// Parent has verified the exact owner/actuator identities before this loss.
	os.Exit(0)
	return nil
}

func admissionNegativeCommand(ctx context.Context, client *Client, role, base string) error {
	switch role {
	case "managed-repair", "managed-repair-restore":
		return client.replaceQueuedRunnerCommand(ctx, 7, "old command", "replacement")
	case "status", "log", "edit":
		args := []string{role}
		if role != "status" {
			args = append(args, "1")
		}
		_, err := client.command(ctx, nil, args...)
		return err
	case "task-fixture", "provider-fixture", "editor-fixture", "pager-fixture":
		helper := os.Getenv("DELEGATE_ADMISSION_LEASE_HELPER")
		if !filepath.IsAbs(helper) || filepath.Clean(helper) != helper {
			return ErrConfiguration
		}
		command := exec.Command(helper, "-test.run=^TestManagedShutdownLeaseFixture$", "-test.timeout=45s")
		command.Dir = base
		command.Env = []string{"DELEGATE_MANAGED_SHUTDOWN_ROLE=1", "DELEGATE_MANAGED_SHUTDOWN_BASE=" + base, "DELEGATE_MANAGED_SHUTDOWN_SECRET=" + os.Getenv("DELEGATE_MANAGED_SHUTDOWN_SECRET"), "DELEGATE_MANAGED_SHUTDOWN_DEADLINE=" + os.Getenv("DELEGATE_ADMISSION_LEASE_DEADLINE"), "GORACE=atexit_sleep_ms=0"}
		pending := startControlOwnedCaptured(ctx, client.options.ControlOwnerExecutable, command, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", client.options.Observer, "")
		_, err := awaitPending(ctx, pending)
		return err
	default:
		return ErrConfiguration
	}
}

func admissionNegativeObserver(base, role string, started chan<- error) func(CommandEvent) {
	return func(event CommandEvent) {
		if event.Stage != "started" {
			return
		}
		ref, err := lifetime.ObserveProcess(event.PID)
		if err == nil {
			data, marshalErr := task.MarshalCanonical(struct {
				PID     int    `json:"pid"`
				Token   string `json:"token"`
				Command string `json:"command"`
			}{PID: ref.PID(), Token: ref.StartToken(), Command: role})
			err = marshalErr
			if err == nil {
				err = publishPrivateRecord(base, filepath.Join(base, "active-cli.json"), data)
			}
		}
		select {
		case started <- err:
		default:
		}
	}
}
