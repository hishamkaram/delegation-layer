package testsupport

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

func (f *RecordFixture) CloseTask(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.client.CloseTask(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func (f *RecordFixture) Hello(t *testing.T) {
	t.Helper()
	client, err := guardian.NewWorkerClient(f.connection, productionRecordGuardianScope{}, f.taskID, f.deadline,
		func(*exec.Cmd) (guardian.LaunchGate, error) { return productionRecordGuardianGate{}, nil })
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	f.client = client
}

func (f *RecordFixture) RunCommand(t *testing.T) {
	t.Helper()
	command := exec.Command("unused-lifetime-provider")
	if err := f.client.StartCommand(command, func() error {
		command.Process = &os.Process{Pid: 101}
		return nil
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.client.FinishCommand(ctx, command); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func (f *RecordFixture) WaitRun(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.runDone:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("production record guardian did not finish")
		return nil
	}
}

func (w *productionRecordGuardianWorker) release() {
	w.once.Do(func() { close(w.done) })
}

func (w *productionRecordGuardianWorker) setWaitError(err error) {
	w.mu.Lock()
	w.waitErr = err
	w.mu.Unlock()
}

func (w *productionRecordGuardianWorker) start(cmd *exec.Cmd) error {
	cmd.Process = &os.Process{Pid: 303}
	return nil
}

func (w *productionRecordGuardianWorker) wait(*exec.Cmd) error {
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waitErr
}

const TaskID = "0123456789abcdef0123456789abcdef"

func NewRecordFixture(
	t *testing.T,
	rootID, taskID, ownerKind string,
	deadline time.Time,
	records guardian.Records,
	acquireOwner func(context.Context) (guardian.Owner, error),
	configure func(*guardian.Options),
) *RecordFixture {
	t.Helper()
	parent, child := net.Pipe()
	worker := &productionRecordGuardianWorker{done: make(chan struct{})}
	options := productionRecordGuardianOptions(t, parent, worker)
	options.RootID, options.TaskID, options.OwnerKind = rootID, taskID, ownerKind
	options.Deadline, options.Records, options.AcquireOwner = deadline, records, acquireOwner
	if configure != nil {
		configure(&options)
	}
	owner, err := guardian.New(options)
	if err != nil {
		t.Fatal(errors.Join(err, parent.Close(), child.Close()))
	}
	done, exited := make(chan error, 1), make(chan struct{})
	fixture := &RecordFixture{
		connection: child, worker: worker, taskID: taskID, deadline: deadline,
		runDone: done,
	}
	go func() {
		done <- owner.Run(t.Context())
		close(exited)
	}()
	t.Cleanup(func() {
		worker.release()
		if fixture.client != nil {
			if err := fixture.client.Close(); err != nil {
				t.Errorf("close production record worker client: %v", err)
			}
		}
		CloseConnection(t, child)
		CloseConnection(t, parent)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("production record guardian did not finish cleanup")
			// Store cleanup must not race an owner that is still finalizing.
			<-exited
		}
	})
	return fixture
}

type RecordFixture struct {
	client     *guardian.WorkerClient
	connection net.Conn
	worker     *productionRecordGuardianWorker
	taskID     string
	deadline   time.Time
	runDone    <-chan error
}

type productionRecordGuardianGate struct{}

func (productionRecordGuardianGate) Abort() error { return nil }

func (productionRecordGuardianGate) Release() error { return nil }

func (productionRecordGuardianGate) Started(error) error { return nil }

func productionRecordGuardianOptions(t *testing.T, connection net.Conn, worker *productionRecordGuardianWorker) guardian.Options {
	t.Helper()
	ref, err := lifetime.NewRef(lifetime.KindProcessGroup, lifetime.AssuranceCooperative, "test-scope")
	if err != nil {
		t.Fatal(err)
	}
	return guardian.Options{
		Connection: connection,
		Worker:     exec.Command("unused-lifetime-worker"),
		PlanFactory: func(string, string, string) (lifetime.Plan, error) {
			return lifetime.NewPlan(lifetime.KindProcessGroup, lifetime.AssuranceCooperative, "pg:00000000000000000000000000000000")
		},
		ScopeFactory: func(lifetime.Plan) (guardian.Scope, error) {
			return productionRecordGuardianScope{ref: ref}, nil
		},
		BootID:      func() (string, error) { return "test-boot", nil },
		StartWorker: worker.start,
		WaitWorker:  worker.wait,
		ObserveWorker: func(*exec.Cmd) (lifetime.ProcessRef, error) {
			return lifetime.NewProcessRef(303, "worker-start-token")
		},
		ReconcileOwned:      func(context.Context) error { return nil },
		WaitRecordRetry:     func(context.Context) error { return nil },
		FinalizationBackoff: func(context.Context, time.Duration) error { return nil },
		CleanupTimeout:      time.Second,
	}
}

type productionRecordGuardianScope struct{ ref lifetime.Ref }

func (productionRecordGuardianScope) BindOwnerProcess() error { return nil }

func (productionRecordGuardianScope) Configure(*exec.Cmd) error { return nil }

func (productionRecordGuardianScope) DestroyEmpty() error { return nil }

func (productionRecordGuardianScope) EmptyContext(context.Context, []lifetime.LaunchRef) (bool, error) {
	return true, nil
}

func (productionRecordGuardianScope) ObserveStarted(pid int) (lifetime.LaunchRef, error) {
	return lifetime.NewLaunchRef(pid, "test-start", pid)
}

func (productionRecordGuardianScope) PendingStartEmptyContext(context.Context) (bool, error) {
	return true, nil
}

func (s productionRecordGuardianScope) Reference() lifetime.Ref { return s.ref }

func (productionRecordGuardianScope) Release() error { return nil }

func (productionRecordGuardianScope) Terminate(context.Context, []lifetime.LaunchRef) error {
	return nil
}

func (productionRecordGuardianScope) TrackOwnerProcess(lifetime.ProcessRef) error { return nil }

type productionRecordGuardianWorker struct {
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	waitErr error
}

func CloseConnection(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close parent connection: %v", err)
	}
}

func (f *RecordFixture) ReleaseWorker() { f.worker.release() }

func (f *RecordFixture) SetWorkerWaitError(err error) { f.worker.setWaitError(err) }
