package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This worker follows the production ordering: open the registered lifetime,
// authenticate hello, then obtain the one-use start permit. A missing project
// closes the worker client through the ordinary Close path, without CloseTask.
func TestPathConnectorMissingWorkerHelper(t *testing.T) {
	root := os.Getenv("DELEGATE_PATH_MISSING_ROOT")
	if root == "" {
		return
	}
	if err := runPathConnectorMissingWorker(root, os.Getenv("DELEGATE_PATH_MISSING_TASK"), os.Getenv("DELEGATE_PATH_MISSING_PROOF")); err != nil {
		t.Fatal(err)
	}
}

func runPathConnectorMissingWorker(root, id, proof string) (resultErr error) {
	store, err := taskdir.OpenStore(root)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	directory, err := store.OpenTask(id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	client, err := pathConnectorWorkerClient(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, client.Close()) }()
	permit, err := directory.PrepareStart(0)
	if permit != nil {
		return errors.Join(errors.New("missing path unexpectedly obtained a start permit"), err, permit.Release())
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("missing project refusal: %w", err)
	}
	return os.WriteFile(proof, []byte("missing-project-refused\n"), 0o600)
}

func pathConnectorWorkerClient(directory *taskdir.TaskDir) (_ *guardian.WorkerClient, resultErr error) {
	registration, err := directory.ReadLifetimeRegistration()
	if err != nil {
		return nil, err
	}
	intent, err := directory.ReadLifetimeIntent()
	if err != nil {
		return nil, err
	}
	if !task.LifetimeIntentMatchesRegistration(intent, registration) {
		return nil, task.ErrIdentityMismatch
	}
	plan, err := lifetime.NewPlan(lifetime.Kind(intent.ScopeKind), lifetime.Assurance(intent.Assurance), intent.ScopePlan)
	if err != nil {
		return nil, err
	}
	ref, err := lifetime.NewRef(lifetime.Kind(registration.ScopeKind), lifetime.Assurance(registration.Assurance), registration.ScopeID)
	if err != nil {
		return nil, err
	}
	scope, err := lifetime.OpenRegistered(plan, ref)
	if err != nil {
		return nil, err
	}
	connection, err := nativeConnection(os.NewFile(3, "path-missing-worker-control"))
	if err != nil {
		return nil, errors.Join(err, scope.Release())
	}
	deadline, err := time.Parse(time.RFC3339Nano, registration.Deadline)
	if err != nil {
		return nil, errors.Join(err, connection.Close(), scope.Release())
	}
	runner, err := os.Executable()
	if err != nil {
		return nil, errors.Join(err, connection.Close(), scope.Release())
	}
	client, err := guardian.NewWorkerClient(connection, scope, directory.TaskID, deadline, guardian.NewLaunchGateFactory(runner))
	if err != nil {
		return nil, errors.Join(err, connection.Close(), scope.Release())
	}
	return client, nil
}

func TestPathConnectorMissingBeforeStartClosesNativeGuardianAndRefusesReplay(t *testing.T) {
	directory, plan, providerEvidence := connectorCwdPreparedTaskBinding(t, true, true)
	request, _, err := directory.PreparedRecords()
	require(t, err)
	require(t, os.Rename(plan.Directory, plan.Directory+"-moved"))
	proof := filepath.Join(t.TempDir(), "worker-refusal.txt")
	connection, child, err := nativeSocketPair()
	require(t, err)
	t.Cleanup(func() {
		if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		if closeErr := child.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Error(closeErr)
		}
	})
	command := exec.Command(plan.Executable, "-test.run=^TestPathConnectorMissingWorkerHelper$")
	command.ExtraFiles = []*os.File{child}
	command.Env = append(os.Environ(),
		"DELEGATE_PATH_MISSING_ROOT="+filepath.Dir(filepath.Dir(directory.Dir)),
		"DELEGATE_PATH_MISSING_TASK="+directory.TaskID,
		"DELEGATE_PATH_MISSING_PROOF="+proof,
	)
	var diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &diagnostic, &diagnostic
	var starts, waits atomic.Int32
	var childCloseErr error
	options := guardian.Options{
		RootID: request.RootID, TaskID: directory.TaskID, OwnerKind: task.LifetimeOwnerTask,
		Deadline: time.Now().Add(time.Minute), Records: directory, Worker: command, Connection: connection,
		AcquireOwner:   func(ctx context.Context) (guardian.Owner, error) { return directory.AcquireLifetimeOwner(ctx) },
		ReconcileOwned: func(ctx context.Context) error { return guardian.ReconcileTaskLifetimeOwned(ctx, directory) },
		StartWorker: func(worker *exec.Cmd) error {
			starts.Add(1)
			startErr := worker.Start()
			childCloseErr = child.Close()
			return startErr
		},
		WaitWorker: func(worker *exec.Cmd) error { waits.Add(1); return worker.Wait() },
	}
	owner, err := guardian.New(options)
	require(t, err)
	if runErr := owner.Run(t.Context()); runErr == nil {
		t.Fatal("worker control loss was not reported")
	}
	require(t, childCloseErr)
	if starts.Load() != 1 || waits.Load() != 1 {
		t.Fatalf("native worker Start/Wait = %d/%d, want 1/1", starts.Load(), waits.Load())
	}
	data, err := os.ReadFile(proof)
	if err != nil || string(data) != "missing-project-refused\n" {
		t.Fatalf("worker did not reach the missing-project boundary: proof=%q err=%v diagnostic=%s", data, err, diagnostic.String())
	}
	assertPathConnectorNativeClosure(t, directory)
	for _, path := range []string{providerEvidence, filepath.Join(directory.Dir, "provider.start"), filepath.Join(directory.Dir, "raw"), filepath.Join(directory.Dir, "outcome.json")} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("pre-start refusal created provider evidence %q: %v", filepath.Base(path), statErr)
		}
	}
	require(t, os.Mkdir(plan.Directory, 0o700))
	replay, err := guardian.New(options)
	require(t, err)
	if err = replay.Run(t.Context()); !errors.Is(err, task.ErrTerminalTask) || starts.Load() != 1 || waits.Load() != 1 {
		t.Fatalf("restored project replayed a closed lifetime: err=%v worker Start/Wait=%d/%d", err, starts.Load(), waits.Load())
	}
}

func assertPathConnectorNativeClosure(t *testing.T, directory *taskdir.TaskDir) {
	t.Helper()
	worker, err := directory.ReadLifetimeWorker()
	require(t, err)
	wait, err := directory.ReadLifetimeWorkerWait()
	require(t, err)
	termination, err := directory.ReadLifetimeTermination()
	require(t, err)
	launches, err := directory.ReadLifetimeLaunches()
	require(t, err)
	if wait.ChildPID != worker.PID || wait.Result != task.LifetimeWorkerWaitSuccess || !termination.ScopeEmpty || termination.Cause != task.LifetimeTerminationRecovery || len(launches) != 0 {
		t.Fatalf("missing project did not close its exact worker and empty scope: worker=%+v Wait=%+v termination=%+v launches=%d", worker, wait, termination, len(launches))
	}
	free, err := directory.RunnerLeaseReleased()
	require(t, err)
	if !free {
		t.Fatal("missing-project worker retained its run lease")
	}
	owner, err := directory.TryAcquireLifetimeOwnerContext(t.Context())
	require(t, err)
	require(t, owner.Release())
	registration, err := directory.ReadLifetimeRegistration()
	require(t, err)
	t.Logf("native closure backend=%s assurance=%s worker=%d", registration.ScopeKind, registration.Assurance, worker.PID)
}
