package app

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"golang.org/x/sys/unix"
)

const lifetimeChildFD = 3

type lifetimeGuardianRunState struct {
	ownerKind      string
	records        lifetimeGuardianRecords
	acquireOwner   func(context.Context) (lifetimeGuardianOwner, error)
	reconcileOwned func(context.Context) error
	userStop       func() (bool, error)
	deadline       time.Time
	closeOwner     func() error
}

func loadLifetimeGuardianRunState(store *taskdir.Store, parsed runnerArguments) (lifetimeGuardianRunState, error) {
	state := lifetimeGuardianRunState{ownerKind: task.LifetimeOwnerTask}
	if parsed.WorkspaceOperation {
		return loadWorkspaceGuardianState(store, parsed.TaskID)
	}
	if parsed.Inspection {
		operation, err := loadInspectionOperationForWorker(store, parsed.TaskID)
		if err != nil {
			return lifetimeGuardianRunState{}, err
		}
		state.ownerKind = task.LifetimeOwnerInspection
		state.records = operation
		state.acquireOwner = func(ctx context.Context) (lifetimeGuardianOwner, error) {
			return operation.AcquireLifetimeOwner(ctx)
		}
		state.reconcileOwned = func(ctx context.Context) error {
			return reconcileInspectionLifetimeOwned(ctx, operation)
		}
		state.deadline = operation.Deadline()
		state.closeOwner = operation.Close
		return state, nil
	}

	td, err := store.OpenTask(parsed.TaskID)
	if err != nil {
		return lifetimeGuardianRunState{}, err
	}
	req, _, preparedErr := td.PreparedRecords()
	if preparedErr != nil {
		return lifetimeGuardianRunState{}, errors.Join(preparedErr, td.Close())
	}
	if req.BudgetNanos <= 0 {
		return lifetimeGuardianRunState{}, errors.Join(task.ErrEvidenceFault, td.Close())
	}
	state.records = td
	state.acquireOwner = func(ctx context.Context) (lifetimeGuardianOwner, error) {
		return td.AcquireLifetimeOwner(ctx)
	}
	state.reconcileOwned = func(ctx context.Context) error {
		return reconcileTaskLifetimeOwned(ctx, td)
	}
	state.userStop = func() (bool, error) {
		return taskUserStopRequested(td)
	}
	state.deadline = time.Now().Add(time.Duration(req.BudgetNanos))
	state.closeOwner = td.Close
	return state, nil
}

// runLifetimeGuardianMode is the composition boundary between scheduler and
// process lifetime ownership. Pueue starts this guardian; the guardian alone
// owns the child worker, containment scope, and durable empty-scope proof.
func runLifetimeGuardianMode(parsed runnerArguments, deps Dependencies) (resultErr error) {
	canonicalRoot, err := resolveRoot(parsed.Root)
	if err != nil {
		return err
	}
	deps = deps.normalized()
	store, err := openStore(canonicalRoot, deps.storeDependencies(), false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	parentConnection, childFile, err := newLifetimeControlPair()
	if err != nil {
		return err
	}
	childOwned := true
	defer func() {
		resultErr = errors.Join(resultErr, closeLifetimeConnection(parentConnection))
		if childOwned {
			resultErr = errors.Join(resultErr, childFile.Close())
		}
	}()

	worker, err := newLifetimeWorkerCommand(executable, canonicalRoot, parsed)
	if err != nil {
		return err
	}
	worker.ExtraFiles = []*os.File{childFile}
	worker.Stdout = os.Stdout
	worker.Stderr = os.Stderr

	state, err := loadLifetimeGuardianRunState(store, parsed)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, state.closeOwner()) }()

	var inheritedCloseErr error
	guardian, err := newLifetimeGuardian(lifetimeGuardianOptions{
		rootID:         store.RootID,
		taskID:         parsed.TaskID,
		ownerKind:      state.ownerKind,
		deadline:       state.deadline,
		records:        state.records,
		acquireOwner:   state.acquireOwner,
		reconcileOwned: state.reconcileOwned,
		userStop:       state.userStop,
		worker:         worker,
		connection:     parentConnection,
		startWorker: func(cmd *exec.Cmd) error {
			startErr := cmd.Start()
			closeErr := childFile.Close()
			if closeErr == nil || errors.Is(closeErr, os.ErrClosed) {
				childOwned = false
			}
			if startErr != nil {
				return errors.Join(startErr, closeErr)
			}
			// A successfully started child remains owned even if releasing the
			// parent's duplicate descriptor reports an error. Preserve that
			// cleanup fault without misclassifying the process as never started.
			inheritedCloseErr = closeErr
			return nil
		},
	})
	if err != nil {
		return err
	}
	guardianErr := runGuardianWithWorkspaceCompletion(guardian, store, parsed, deps)
	return errors.Join(guardianErr, inheritedCloseErr)
}

func runGuardianWithWorkspaceCompletion(guardian *lifetimeGuardian, store *taskdir.Store, parsed runnerArguments, deps Dependencies) error {
	guardianErr := guardian.Run(context.Background())
	if parsed.Inspection {
		return guardianErr
	}
	if wakeErr := reconcileWorkspaceAfterGuardian(store, parsed, deps); wakeErr != nil && !errors.Is(wakeErr, workspace.ErrPending) {
		// Capture remains recoverable without rewriting the provider result.
		log.Printf("delegate: workspace capture deferred: %v", wakeErr)
	}
	return guardianErr
}

func newLifetimeWorkerCommand(executable, canonicalRoot string, parsed runnerArguments) (*exec.Cmd, error) {
	if !filepath.IsAbs(executable) || !filepath.IsAbs(canonicalRoot) {
		return nil, task.ErrEvidenceFault
	}
	parsed.Root = canonicalRoot
	worker := exec.Command(executable, lifetimeWorkerArguments(parsed)...)
	worker.Dir = canonicalRoot
	return worker, nil
}

func lifetimeWorkerArguments(parsed runnerArguments) []string {
	arguments := make([]string, 0, 9)
	if parsed.Inspection {
		arguments = append(arguments, "--inspection")
	}
	if parsed.WorkspaceOperation {
		arguments = append(arguments, "--workspace-operation")
	}
	arguments = append(
		arguments,
		"--lifetime-child",
		"--lifetime-fd", strconv.Itoa(lifetimeChildFD),
		"--supervised",
		"--root", parsed.Root,
		parsed.TaskID,
	)
	return arguments
}

func closeLifetimeConnection(connection net.Conn) error {
	if connection == nil {
		return nil
	}
	err := connection.Close()
	if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func newLifetimeControlPair() (net.Conn, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	parentFile := os.NewFile(uintptr(fds[0]), "delegate-lifetime-parent")
	childFile := os.NewFile(uintptr(fds[1]), "delegate-lifetime-child")
	if parentFile == nil || childFile == nil {
		var closeErr error
		if parentFile != nil {
			closeErr = errors.Join(closeErr, parentFile.Close())
		} else {
			closeErr = errors.Join(closeErr, unix.Close(fds[0]))
		}
		if childFile != nil {
			closeErr = errors.Join(closeErr, childFile.Close())
		} else {
			closeErr = errors.Join(closeErr, unix.Close(fds[1]))
		}
		return nil, nil, errors.Join(task.ErrEvidenceFault, closeErr)
	}
	parentConnection, err := net.FileConn(parentFile)
	closeErr := parentFile.Close()
	if err != nil || closeErr != nil {
		if parentConnection != nil {
			closeErr = errors.Join(closeErr, parentConnection.Close())
		}
		return nil, nil, errors.Join(err, closeErr, childFile.Close())
	}
	return parentConnection, childFile, nil
}

var (
	_ lifetimeGuardianRecords = (*taskdir.TaskDir)(nil)
	_ lifetimeGuardianRecords = (*inspection.Operation)(nil)
	_ lifetimeGuardianRecords = (*workspace.Operation)(nil)
)
