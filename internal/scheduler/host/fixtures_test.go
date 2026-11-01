package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--host-fixture-child":
			os.Exit(fixtureChild())
		case "--host-fixture-serve":
			os.Exit(fixtureHost())
		case "--host-fixture-fdprobe":
			os.Exit(fixtureFDProbeResult())
		}
	}
	os.Exit(m.Run())
}

func fixtureConfig(store *taskdir.Store) (Config, error) {
	executable, err := os.Executable()
	if err != nil {
		return Config{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return Config{}, err
	}
	build, err := verifiedexec.Fingerprint(executable)
	if err != nil {
		return Config{}, err
	}
	directory, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}
	environment := append(os.Environ(), "DL_HOST_ROOT="+store.Root)
	return Config{Store: store, Executable: executable, Args: []string{"--host-fixture-child"}, Environment: environment, Directory: directory, BuildID: build}, nil
}

func fixtureChild() (exit int) {
	store, err := taskdir.OpenStore(os.Getenv("DL_HOST_ROOT"))
	if err != nil {
		return 90
	}
	defer func() {
		if store.Close() != nil {
			exit = 91
		}
	}()
	execution, err := scheduler.AcquireOwnerLease(context.Background(), store)
	if errors.Is(err, task.ErrLockBusy) {
		return ChildBusyExitCode
	}
	if err != nil {
		return 92
	}
	defer func() {
		if execution.Close() != nil {
			exit = 93
		}
	}()
	config, err := fixtureConfig(store)
	if err != nil {
		return 94
	}
	child := ChildConfig{RootID: store.RootID, BuildID: config.BuildID}
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, ChildReadFDFlag+"=") {
			child.ReadFD, err = strconv.Atoi(strings.TrimPrefix(arg, ChildReadFDFlag+"="))
		} else if strings.HasPrefix(arg, ChildWriteFDFlag+"=") {
			child.WriteFD, err = strconv.Atoi(strings.TrimPrefix(arg, ChildWriteFDFlag+"="))
		}
		if err != nil {
			return 95
		}
	}
	child.BeforeReady = fixturePreflight
	err = RunChild(context.Background(), child, fixtureRuntime)
	if err != nil {
		return 96
	}
	return 0
}

func fixturePreflight(ctx context.Context) error {
	switch os.Getenv("DL_HOST_MODE") {
	case "permanent":
		return fmt.Errorf("fixture schema fault: %w", ErrIncompatible)
	case "unready":
		<-ctx.Done()
		return ctx.Err()
	case "transient":
		return errors.New("fixture temporary preflight fault")
	case "gated_permanent":
		directory := os.Getenv("DL_HOST_MARKERS")
		if err := os.WriteFile(filepath.Join(directory, "preflight-entered"), []byte("entered"), 0o600); err != nil {
			return err
		}
		for {
			if _, err := os.Stat(filepath.Join(directory, "preflight-release")); err == nil {
				return ErrIncompatible
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			timer := time.NewTimer(time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}

func fixtureRuntime(ctx context.Context) error {
	if os.Getenv("DL_HOST_MODE") == "fd_probe" {
		if err := fixtureFDProbe(); err != nil {
			return err
		}
	}
	if os.Getenv("DL_HOST_MODE") == "pueue" {
		if err := fixturePueue(ctx); err != nil {
			return err
		}
	}
	switch os.Getenv("DL_HOST_MODE") {
	case "runtime_permanent":
		return fmt.Errorf("fixture runtime invariant: %w", ErrIncompatible)
	case "crash":
		os.Exit(23)
	case "exit":
		return nil
	}
	if directory := os.Getenv("DL_HOST_MARKERS"); directory != "" {
		value, present := os.LookupEnv("DL_HOST_VISIBLE")
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		sid, err := unix.Getsid(0)
		if err != nil {
			return err
		}
		content := fmt.Sprintf("%t\n%s\n%s\n%d", present, value, cwd, sid)
		pending := filepath.Join(directory, "context.pending")
		if err = os.WriteFile(pending, []byte(content), 0o600); err != nil {
			return err
		}
		// The final path is the fixture's publication boundary for its reader.
		if err = os.Rename(pending, filepath.Join(directory, "context")); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}

func fixtureHost() (exit int) {
	store, err := taskdir.OpenStore(os.Getenv("DL_HOST_ROOT"))
	if err != nil {
		return 80
	}
	defer func() {
		if store.Close() != nil {
			exit = 81
		}
	}()
	config, err := fixtureConfig(store)
	if err != nil {
		return 82
	}
	input, err := inheritedPipe(3, "fixture-host-owner", unix.O_RDONLY)
	if err != nil {
		return 83
	}
	ctx, cancel := context.WithCancel(context.Background())
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		var command [1]byte
		if _, readErr := io.ReadFull(input, command[:]); readErr == nil && command[0] == 'x' {
			os.Exit(23)
		}
		cancel()
	}()
	err = Serve(ctx, config)
	cancel()
	closeErr := input.Close()
	<-joined
	if err != nil || closeErr != nil {
		return 84
	}
	return 0
}

func queryFor(t *testing.T, config Config) Query {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), config.Store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return Query{Store: reader, BuildID: config.BuildID}
}

func testConfig(t *testing.T, mode string) Config {
	t.Helper()
	config, err := fixtureConfig(stateStore(t))
	if err != nil {
		t.Fatal(err)
	}
	config.Environment = append(config.Environment, "DL_HOST_MODE="+mode)
	return config
}

func eventuallyStatus(t *testing.T, query Query, predicate func(Status) bool) Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var last Status
	var lastErr error
	for ctx.Err() == nil {
		last, lastErr = Observe(ctx, query)
		if lastErr == nil && predicate(last) {
			return last
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	t.Fatalf("host state gate did not arrive: status=%+v err=%v", last, lastErr)
	return Status{}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for host ownership gate")
	}
	var zero T
	return zero
}

func runLocalHost(t *testing.T, config Config, clock clockwork.Clock) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); done <- serve(ctx, config, clock) }()
	t.Cleanup(func() { cancel(); receive(t, joined) })
	return done
}

type fixtureProcess struct {
	control  *os.File
	done     chan struct{}
	err      error // Immutable after done closes; only the Wait goroutine writes it.
	once     sync.Once
	closeErr error
}

func (p *fixtureProcess) release() error {
	p.once.Do(func() { p.closeErr = p.control.Close() })
	return p.closeErr
}

func startHostFixture(t *testing.T, config Config) *fixtureProcess {
	t.Helper()
	input, control, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(config.Executable, "--host-fixture-serve")
	cmd.Env, cmd.Dir, cmd.ExtraFiles = config.Environment, config.Directory, []*os.File{input}
	if err = cmd.Start(); err != nil {
		t.Fatal(errors.Join(err, input.Close(), control.Close()))
	}
	process := &fixtureProcess{control: control, done: make(chan struct{})}
	go func() { process.err = cmd.Wait(); close(process.done) }()
	t.Cleanup(func() {
		if closeErr := process.release(); closeErr != nil {
			t.Error(closeErr)
		}
		receive(t, process.done)
	})
	if err = input.Close(); err != nil {
		t.Fatal(err)
	}
	return process
}
