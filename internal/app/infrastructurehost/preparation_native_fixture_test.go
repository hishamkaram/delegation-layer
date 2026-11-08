package infrastructurehost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This finite fixture owns actual fake-CLI daemon Waits and every borrower.
// It supplies explicit root-bound ports, never native placement authority.
type preparationProbe struct {
	mu                       sync.Mutex
	base, root               string
	closing                  bool
	waits, pending           []<-chan struct{}
	clients                  []*pueue.Client
	store                    *taskdir.Store
	directory, rootDirectory *os.File
	listener                 net.Listener
	listenerDone             chan struct{}
}

func newOwnedPreparationProbe(t *testing.T) (*taskdir.Store, *pueue.Client, *preparationProbe) {
	t.Helper()
	owner := &preparationProbe{}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if err := owner.close(ctx); err != nil {
			t.Errorf("retained preparation probe %s: %v", owner.base, err)
		}
	})
	base, err := os.MkdirTemp("", "dl-ih-")
	if err != nil {
		t.Fatal(err)
	}
	owner.base = base
	owner.base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	owner.root = filepath.Join(owner.base, "state")
	owner.directory, err = os.Open(owner.base)
	if err != nil {
		t.Fatal(err)
	}
	owner.store, err = taskdir.InitStore(owner.root)
	if err != nil {
		t.Fatal(err)
	}
	owner.rootDirectory, err = os.Open(owner.root)
	if err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(owner.base, "bundle", "pueue")
	daemonPath := filepath.Join(owner.base, "bundle", "pueued")
	writePreparationProbeExecutables(t, owner.store.RootID, clientPath, daemonPath)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := pueue.BindPrivate(ctx, clientPath, daemonPath, owner.root, pueue.Options{
		ObservationTimeout: 10 * time.Second, WaitForPendingClose: true,
		DaemonStartGuard: owner, PrivateGenerationGuard: owner, DaemonWaitOwner: owner,
	})
	owner.ownClient(client, err)
	if err != nil {
		t.Fatalf("bind healthy private supervisor fixture: %v", err)
	}
	path, unix := strings.CutPrefix(client.Binding().Endpoint, "unix:")
	if !unix || !filepath.IsAbs(path) {
		t.Fatal("preparation probe requires absolute Unix endpoint")
	}
	owner.listener, err = net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	owner.listenerDone = make(chan struct{})
	go func() {
		defer close(owner.listenerDone)
		for {
			connection, acceptErr := owner.listener.Accept()
			if acceptErr != nil {
				return
			}
			if closeErr := connection.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	}()
	return owner.store, client, owner
}

func (f *preparationProbe) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return f.invoke(ctx, root, start)
}

func (f *preparationProbe) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	return f.invoke(ctx, root, commit)
}

func (f *preparationProbe) invoke(ctx context.Context, root string, callback func() error) error {
	if ctx == nil || callback == nil {
		return os.ErrInvalid
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.closing || root != f.root || root == "" {
		return pueue.ErrInfrastructureOwnerRequired
	}
	return callback()
}

func (f *preparationProbe) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if root != f.root || pid <= 0 || done == nil {
		return os.ErrInvalid
	}
	f.waits = append(f.waits, done)
	return nil
}

func (f *preparationProbe) ownClient(client *pueue.Client, cause error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if client != nil && !slices.Contains(f.clients, client) {
		f.clients = append(f.clients, client)
	}
	var inFlight *pueue.InFlightError
	if errors.As(cause, &inFlight) && inFlight.Pending != nil {
		f.pending = append(f.pending, inFlight.Pending.Done())
	}
}

func (f *preparationProbe) close(ctx context.Context) error {
	f.mu.Lock()
	f.closing = true
	waits := append(slices.Clone(f.pending), f.waits...)
	f.mu.Unlock()
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("preparation probe Wait unresolved: %w", ctx.Err())
		}
	}
	if err := f.closeBorrowers(); err != nil {
		return err
	}
	return f.removeOwnedFiles()
}

func (f *preparationProbe) closeBorrowers() error {
	if f.listener != nil {
		if err := f.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
		<-f.listenerDone
	}
	for _, client := range f.clients {
		if err := client.Close(); err != nil {
			return err
		}
	}
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (f *preparationProbe) removeOwnedFiles() error {
	for _, pin := range []struct {
		directory *os.File
		path      string
	}{{f.rootDirectory, f.root}, {f.directory, f.base}} {
		if pin.directory == nil {
			continue
		}
		held, err := pin.directory.Stat()
		if err != nil {
			return err
		}
		current, err := os.Lstat(pin.path)
		if err != nil {
			return err
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(held, current) {
			return errors.New("preparation probe directory identity changed")
		}
		if err = pin.directory.Close(); err != nil {
			return err
		}
	}
	if f.base == "" {
		return nil
	}
	return os.RemoveAll(f.base)
}

func refusingCallbacks(t *testing.T) Callbacks {
	t.Helper()
	refuse := func(name string) error {
		t.Errorf("unexpected native callback %s", name)
		return errors.New("unexpected native callback: " + name)
	}
	return Callbacks{
		MaintenanceLaunch: func(context.Context, string, string, []string) error { return refuse("maintenance") },
		SuccessfulCleanup: func(context.Context, string, task.SupervisorRef, []pueue.Job) (pueue.PrivateQueueCleanupPlan, error) {
			return pueue.PrivateQueueCleanupPlan{}, refuse("cleanup")
		},
		TerminalEvidence: func(context.Context, string, task.SupervisorRef, []pueue.Job) error { return refuse("evidence") },
		TerminalCustody: func(context.Context, string, task.SupervisorRef, pueue.PrivateTerminalCustodyScope) error {
			return refuse("custody")
		},
		MigrationAuthorization: func(context.Context, string, task.SupervisorRef, []pueue.Job) error { return refuse("migration") },
	}
}

func readPreparationProbeFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertOrdinarySubmissionCommandCount(t *testing.T, path, command string, want int) {
	t.Helper()
	data := readPreparationProbeFile(t, path)
	count := 0
	for _, entry := range strings.Split(string(data), "\n") {
		if entry == command {
			count++
		}
	}
	if count != want {
		t.Fatalf("supervisor command %q count=%d, want %d; log=%q", command, count, want, data)
	}
}

// Fixture scripts are authored test data, never bytes from installed programs.
func writePreparationProbeExecutables(t *testing.T, rootID, clientPath, daemonPath string) {
	t.Helper()
	supervisorfixture.WriteExecutable(t, clientPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
    [ -f "$base/ready" ] || exit 1
	[ ! -f "$base/reject-status" ] || exit 97
	printf '%s\n' status >> "$base/status"
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":%s},"delegation-inspection-`+rootID+`":{"status":"Running","parallel_tasks":1}}}\n' "$parallel"
    ;;
	group)
	  [ -f "$base/ready" ] || exit 1
	  printf '%s\n' group >> "$base/group"
	  parallel=1
	  [ -f "$base/parallel-unlimited" ] && parallel=0
	  printf '{"default":{"status":"Running","parallel_tasks":%s},"delegation-inspection-`+rootID+`":{"status":"Running","parallel_tasks":1}}\n' "$parallel"
	  ;;
	parallel)
	  printf '%s\n' parallel >> "$base/parallel"
	  : > "$base/parallel-unlimited"
	  ;;
	add)
	  [ "${4-}" = --help ] || exit 64
	  printf '%s\n' add-help >> "$base/staged-capability"
	  printf '%s\n' '--stashed --print-task-id'
	  ;;
	enqueue)
	  [ "${4-}" = --help ] || exit 64
	  printf '%s\n' enqueue-help >> "$base/staged-capability"
	  printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
	  ;;
  *) exit 64 ;;
esac
`)
	writePreparationProbeDaemon(t, daemonPath)
}

// Render original fixture source into its selected installation. The shared
// producer never obtains program bytes from an installed executable.
func writePreparationProbeDaemon(t *testing.T, path string) {
	t.Helper()
	supervisorfixture.WriteExecutable(t, path, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
printf '%s\n' started >> "$base/daemon-started"
mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
: > "$base/ready"
`)
}
