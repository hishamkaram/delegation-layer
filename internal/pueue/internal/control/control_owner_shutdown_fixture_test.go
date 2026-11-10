package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"golang.org/x/sys/unix"
)

const qualifiedControlCallerMode = "--test-qualified-control-caller"

// This fixed caller invokes the production command and owner protocol. Native
// fault signals/descriptor observations remain in internal/lifetime tests.
func runQualifiedControlCaller(arguments []string) (resultErr error) {
	if len(arguments) != 6 {
		return errors.New("qualified control fixture requires six fixed arguments")
	}
	deadline, err := strconv.ParseInt(arguments[5], 10, 64)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, deadline))
	defer cancel()
	root, binary, owner, role, helper := arguments[0], arguments[1], arguments[2], arguments[3], arguments[4]
	base := filepath.Join(root, privateSupervisorDirectory)
	files, err := qualifiedCallerLeases(base)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeControlLeaseFiles(files)) }()
	if err = qualifiedCallerConfig(base); err != nil {
		return err
	}
	ctx = WithControlLease(ctx, qualifiedFixtureLease{files: files})
	return WithBootstrapLockContext(ctx, root, func(locked context.Context) error {
		return runQualifiedControlCommand(locked, root, binary, owner, role, helper, time.Unix(0, deadline))
	})
}

type qualifiedFixtureLease struct{ files []*os.File }

func (l qualifiedFixtureLease) DuplicateControlLeaseFiles() ([]*os.File, error) {
	var result []*os.File
	for _, file := range l.files {
		duplicate, err := duplicateCloseOnExec(file, "qualified-control-fixture-lease")
		if err != nil {
			return nil, errors.Join(err, closeControlLeaseFiles(result))
		}
		result = append(result, duplicate)
	}
	return result, nil
}

func qualifiedCallerLeases(base string) (files []*os.File, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeControlLeaseFiles(files))
			files = nil
		}
	}()
	for _, name := range []string{"queue", "task-maintenance", "lifetime", "instance"} {
		file, err := os.OpenFile(filepath.Join(base, name+".lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return files, err
		}
		files = append(files, file)
		if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return files, err
		}
	}
	for _, fd := range []int{3, 4} {
		file := os.NewFile(uintptr(fd), "qualified-caller-ipc")
		if file == nil {
			return files, os.ErrInvalid
		}
		files = append(files, file)
		if err := markControlLeaseCloseOnExec(file); err != nil {
			return files, err
		}
	}
	return files, nil
}

func qualifiedCallerConfig(base string) error {
	if err := os.MkdirAll(filepath.Join(base, "data"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(base, "run"), 0o700); err != nil {
		return err
	}
	secret := os.Getenv("DELEGATE_MANAGED_SHUTDOWN_SECRET")
	if secret == "" {
		return errors.New("qualified native fixture secret missing")
	}
	if err := os.WriteFile(filepath.Join(base, "data", "shared_secret"), []byte(secret), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(base, "pueue.yml"), []byte(privateConfigYAMLWithSocket(base, filepath.Join(base, "s"))), 0o600)
}

func runQualifiedControlCommand(ctx context.Context, root, binary, owner, role, helper string, deadline time.Time) error {
	base := filepath.Join(root, privateSupervisorDirectory)
	options := Options{ControlOwnerExecutable: owner, ObservationTimeout: time.Second, Environment: []string{"HOME=" + root, "PATH=/usr/bin:/bin", "LANG=C"}}
	client, err := Bind(ctx, binary, filepath.Join(base, "pueue.yml"), options)
	if err != nil {
		return errors.Join(err, client.CloseAfterPending(err))
	}
	if role == "shutdown-migration" || role == "shutdown-recovered" {
		client, err = qualifiedCallerCanonicalLegacy(ctx, root, binary, role, client)
		if err != nil {
			return err
		}
	}
	started := make(chan error, 1)
	client.options.Observer = qualifiedCallerObserver(base, started)
	completed := make(chan error, 1)
	go func() {
		var commandErr error
		switch role {
		case "shutdown", "shutdown-migration", "shutdown-recovered":
			_, commandErr = client.ShutdownDispatched(ctx)
		case "status", "log", "edit":
			args := []string{role}
			if role == "log" || role == "edit" {
				args = append(args, "1")
			}
			_, commandErr = client.command(ctx, nil, args...)
		default:
			command := exec.Command(helper, "-test.run=^TestManagedShutdownLeaseFixture$", "-test.timeout=45s")
			command.Dir = root
			command.Env = []string{"DELEGATE_MANAGED_SHUTDOWN_ROLE=1", "DELEGATE_MANAGED_SHUTDOWN_BASE=" + base, "DELEGATE_MANAGED_SHUTDOWN_SECRET=" + os.Getenv("DELEGATE_MANAGED_SHUTDOWN_SECRET"), "DELEGATE_MANAGED_SHUTDOWN_DEADLINE=" + strconv.FormatInt(deadline.UnixNano(), 10), "GORACE=atexit_sleep_ms=0"}
			pending := startControlOwnedCapturedWithConsume(ctx, owner, command, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", client.options.Observer, "", nil, nil)
			_, commandErr = awaitPending(ctx, pending)
		}
		completed <- commandErr
	}()
	if err = waitQualifiedCallerStart(ctx, started, completed); err != nil {
		return err
	}
	if err = waitQualifiedCallerRelease(ctx, base); err != nil {
		return err
	}
	// Deliberate caller loss after native handoff: kernel close only, never
	// bootstrap LOCK_UN. The production managed owner retains its sole Wait.
	os.Exit(0)
	return nil
}

func waitQualifiedCallerStart(ctx context.Context, started, completed <-chan error) error {
	select {
	case err := <-started:
		if err != nil {
			return err
		}
	case err := <-completed:
		return errors.Join(errors.New("native command completed before primary Start observation"), err)
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func qualifiedCallerCanonicalLegacy(ctx context.Context, root, binary, role string, client *Client) (*Client, error) {
	daemon := filepath.Join(filepath.Dir(binary), "pueued")
	if err := attachPrivateDaemon(client, daemon); err != nil {
		return nil, err
	}
	if role == "shutdown-migration" {
		return newPrivateMigrationShutdownClient(root, client.Binding(), privateDaemonIdentityForBinding(client.Binding()), client.options)
	}
	saved := client.Binding()
	base := filepath.Join(root, privateSupervisorDirectory)
	if err := os.WriteFile(filepath.Join(base, "pueue.yml"), []byte(privateConfigYAMLWithSocket(base, filepath.Join(base, "new.sock"))), 0o600); err != nil {
		return nil, err
	}
	current, err := Bind(ctx, binary, filepath.Join(base, "pueue.yml"), client.options)
	if err != nil {
		return nil, errors.Join(err, current.CloseAfterPending(err))
	}
	if err = attachPrivateDaemon(current, daemon); err != nil {
		return nil, errors.Join(err, current.CloseAfterPending(err))
	}
	return bindSavedEndpointMigration(ctx, saved, current)
}

func qualifiedCallerObserver(base string, started chan<- error) func(CommandEvent) {
	return func(event CommandEvent) {
		if event.Stage != "started" {
			return
		}
		ref, err := lifetime.ObserveProcess(event.PID)
		if err == nil {
			err = writeQualifiedFixtureRecord(filepath.Join(base, "cli.json"), qualifiedFixtureIdentity{PID: ref.PID(), Token: ref.StartToken()})
		}
		select {
		case started <- err:
		default:
		}
	}
}

type qualifiedFixtureIdentity struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

func writeQualifiedFixtureRecord(path string, record qualifiedFixtureIdentity) error {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return err
	}
	return publishPrivateRecord(filepath.Dir(path), path, data)
}

func waitQualifiedCallerRelease(ctx context.Context, base string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(base, "abort")); err == nil {
			return errors.New("native fixture launches/observation aborted")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Stat(filepath.Join(base, "managed.release-caller")); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
