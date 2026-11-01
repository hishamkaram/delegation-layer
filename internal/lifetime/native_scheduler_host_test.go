//go:build linux || darwin

package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const nativeSchedulerCLIEnv = "DELEGATE_TEST_SCHEDULER_CLI"

// This fixture owns the commands and their exact Wait, never the host's child
// supervision. The CLI owns host/runtime shutdown and the existing H/E leases.
type nativeSchedulerFixture struct {
	directory *nativeSchedulerDirectory
	cli       string
	root      string
	rootID    string
	commands  []*nativeSchedulerCommand
	hosts     []*nativeSchedulerCommand
	epochs    map[string]bool
}

type nativeSchedulerStatus struct {
	State   string `json:"state"`
	Epoch   string `json:"epoch"`
	Attempt uint64 `json:"attempt"`
	Ready   bool   `json:"ready"`
}

type nativeSchedulerResponse struct {
	RootID   string          `json:"root_id"`
	Error    string          `json:"error"`
	Failure  json.RawMessage `json:"failure"`
	Schedule *struct {
		ID         string `json:"id"`
		Saved      bool   `json:"saved"`
		NextAction string `json:"next_action"`
	} `json:"schedule"`
	Scheduler *nativeSchedulerStatus `json:"scheduler"`
}

func TestNativeSchedulerHostSignals(t *testing.T) {
	cli := os.Getenv(nativeSchedulerCLIEnv)
	if cli == "" {
		t.Skip("UNVERIFIED native scheduler signals: set DELEGATE_TEST_SCHEDULER_CLI to the candidate CLI with colocated delegate-run")
	}
	fixture := &nativeSchedulerFixture{cli: cli, epochs: make(map[string]bool)}
	t.Cleanup(func() { fixture.cleanup(t) })
	if err := fixture.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !t.Run(signal.String(), func(t *testing.T) { fixture.checkSignal(t, signal) }) {
			break // Never start another generation after uncertain closure.
		}
	}
}

func nativeSchedulerCandidate(cli string) error {
	if !filepath.IsAbs(cli) {
		return fmt.Errorf("%s must name an absolute candidate path", nativeSchedulerCLIEnv)
	}
	for _, path := range []string{cli, filepath.Join(filepath.Dir(cli), "delegate-run")} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("candidate executable unavailable: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("candidate CLI and colocated runner must be executable regular files")
		}
	}
	return nil
}

func (f *nativeSchedulerFixture) prepare(ctx context.Context) error {
	if err := nativeSchedulerCandidate(f.cli); err != nil {
		return err
	}
	var err error
	f.directory, err = newNativeSchedulerDirectory()
	if err != nil {
		return err
	}
	f.root = filepath.Join(f.directory.path, "state")
	if err = f.directory.root.Mkdir("workspace", 0o700); err != nil {
		return err
	}
	if err = f.directory.write("brief.md", []byte("Native scheduler signal fixture. No occurrence should run.\n")); err != nil {
		return err
	}
	// Annual January 1 is safely beyond every bounded case. Refuse the small
	// actual boundary window instead of changing clocks or calendar records.
	now := time.Now().UTC()
	next := time.Date(now.Year()+1, time.January, 1, 0, 0, 0, 0, time.UTC)
	if next.Sub(now) < 10*time.Minute {
		return fmt.Errorf("native signal fixture is too close to its annual cron boundary")
	}
	response, err := f.control(ctx, []string{
		"schedule", "create", "--auto", "--cron", "0 0 1 1 *", "--timezone", "UTC",
		"--brief", filepath.Join(f.directory.path, "brief.md"), "--cwd", filepath.Join(f.directory.path, "workspace"),
		"--permission", "read-only", "--budget", "1m",
	}, 3)
	if err != nil {
		return err
	}
	if response.Schedule == nil || !response.Schedule.Saved || response.Schedule.NextAction != "start_required" {
		return fmt.Errorf("candidate did not save the schedule with start_required")
	}
	if err = task.ValidateTaskID(response.Schedule.ID); err != nil {
		return fmt.Errorf("candidate returned an invalid saved schedule ID: %w", err)
	}
	if err = task.ValidateTaskID(response.RootID); err != nil {
		return fmt.Errorf("candidate returned an invalid root ID: %w", err)
	}
	f.rootID = response.RootID
	return nil
}

func (f *nativeSchedulerFixture) checkSignal(t *testing.T, signal syscall.Signal) {
	t.Helper()
	command, err := f.start([]string{"scheduler", "serve"}, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	status, err := f.awaitReady(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if status.Attempt != 1 || status.Epoch == "" || f.epochs[status.Epoch] {
		t.Fatalf("host is not a fresh first attempt: epoch=%q attempt=%d", status.Epoch, status.Attempt)
	}
	f.epochs[status.Epoch] = true
	attempts, err := f.attemptRecords(ctx)
	if err != nil || len(attempts) != len(f.epochs) {
		t.Fatalf("unexpected native host attempt inventory: count=%d err=%v", len(attempts), err)
	}
	if err = nativeSchedulerHostSignal(ctx, command.identity, signal); err != nil {
		t.Fatalf("send verified %s to the exact direct host: %v", signal, err)
	}
	if err = command.wait(ctx); err != nil {
		t.Fatalf("host did not join orderly shutdown after %s: %v", signal, err)
	}
	if err = f.executionFree(ctx); err != nil {
		t.Fatalf("host Wait left scheduler execution owned: %v", err)
	}
	if err = f.remainsStopped(ctx, status.Epoch, attempts); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: exact host Wait exit=0; epoch=%s attempt=%d; E free; no respawn", signal, status.Epoch, status.Attempt)
}

func (f *nativeSchedulerFixture) awaitReady(ctx context.Context, command *nativeSchedulerCommand) (nativeSchedulerStatus, error) {
	for {
		if command.closed() {
			return nativeSchedulerStatus{}, errors.Join(errors.New("host exited before authenticated readiness"), command.wait(ctx))
		}
		status, err := f.status(ctx)
		if err != nil {
			return nativeSchedulerStatus{}, err
		}
		if status.State == "ready" && status.Ready {
			return status, nil
		}
		if err = sleepContext(ctx, 50*time.Millisecond); err != nil {
			return nativeSchedulerStatus{}, fmt.Errorf("public host readiness: %w", err)
		}
	}
}

func (f *nativeSchedulerFixture) status(ctx context.Context) (nativeSchedulerStatus, error) {
	response, err := f.control(ctx, []string{"scheduler", "status"}, 0)
	if err != nil {
		return nativeSchedulerStatus{}, err
	}
	if response.RootID != f.rootID || response.Scheduler == nil {
		return nativeSchedulerStatus{}, fmt.Errorf("public scheduler status lost its root binding or object")
	}
	return *response.Scheduler, nil
}

func (f *nativeSchedulerFixture) attemptRecords(ctx context.Context) (names []string, resultErr error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	names, err = reader.SchedulerHostRecordNamesContext(ctx)
	if err != nil {
		return nil, err
	}
	names = slices.DeleteFunc(names, func(name string) bool { return !strings.HasPrefix(name, "child-") })
	slices.Sort(names)
	return names, nil
}

func (f *nativeSchedulerFixture) executionFree(ctx context.Context) (resultErr error) {
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, f.root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	lease, err := scheduler.AcquireOwnerLease(ctx, store)
	if err != nil {
		return err
	}
	return lease.Close()
}

func (f *nativeSchedulerFixture) remainsStopped(ctx context.Context, epoch string, attempts []string) error {
	until := time.Now().Add(1250 * time.Millisecond) // Beyond the first restart backoff.
	for {
		status, err := f.status(ctx)
		if err != nil {
			return err
		}
		if (status.State != "no_host_observed" && status.State != "stopped") || status.Ready || status.Epoch != epoch {
			return fmt.Errorf("signal left a ready/replaced host: state=%s epoch=%s", status.State, status.Epoch)
		}
		current, err := f.attemptRecords(ctx)
		if err != nil || !slices.Equal(current, attempts) {
			return errors.Join(fmt.Errorf("host attempts changed after exact Wait"), err)
		}
		if !time.Now().Before(until) {
			return f.executionFree(ctx)
		}
		if err = sleepContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

func (f *nativeSchedulerFixture) cleanup(t *testing.T) {
	t.Helper()
	if f.directory == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, host := range f.hosts {
		if !host.closed() {
			t.Errorf("native host needed cooperative stop fallback; captures retained at %s", f.directory.path)
			if _, err := f.control(ctx, []string{"scheduler", "stop"}, 0, 3); err != nil {
				t.Errorf("cooperative scheduler stop: %v", err)
			}
			break
		}
	}
	closed := true
	for _, command := range f.commands {
		if err := command.wait(ctx); err != nil && !command.closed() {
			t.Errorf("owned command Wait remains unknown: %v", err)
			closed = false
		}
	}
	if len(f.hosts) > 0 && closed {
		if err := f.executionFree(ctx); err != nil {
			t.Errorf("scheduler child closure remains unknown: %v", err)
			closed = false
		}
	}
	if err := f.directory.finish(closed && !t.Failed()); err != nil {
		t.Errorf("fixture cleanup retained %s: %v", f.directory.path, err)
	}
	if !closed || t.Failed() {
		t.Logf("RETAINED native scheduler diagnostics: %s", f.directory.path)
	}
}
