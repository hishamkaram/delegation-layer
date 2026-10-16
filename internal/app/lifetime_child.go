package app

import (
	"context"
	"errors"
	"net"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type openedLifetimeChild struct {
	control  *lifetimeWorkerClient
	deadline time.Time
}

type lifetimeRegistrationReader interface {
	ReadLifetimeIntent() (*task.LifetimeIntentRecord, error)
	ReadLifetimeRegistration() (*task.LifetimeRegistrationRecord, error)
}

func openTaskLifetimeChild(parsed runnerArguments, td *taskdir.TaskDir) (*openedLifetimeChild, error) {
	if !parsed.LifetimeChild || parsed.LifetimeFD < 3 || td == nil || parsed.TaskID != td.TaskID {
		return nil, task.ErrEvidenceFault
	}
	return openLifetimeChild(parsed, td)
}

func openInspectionLifetimeChild(parsed runnerArguments, operation *inspection.Operation) (*openedLifetimeChild, error) {
	if !parsed.LifetimeChild || parsed.LifetimeFD < 3 || operation == nil || parsed.TaskID != operation.Request().IdentityID() {
		return nil, task.ErrEvidenceFault
	}
	return openLifetimeChild(parsed, operation)
}

func openLifetimeChild(parsed runnerArguments, records lifetimeRegistrationReader) (*openedLifetimeChild, error) {
	registration, intent, err := readLifetimeChildRecords(records)
	if err != nil {
		return nil, err
	}
	plan, err := lifetime.NewPlan(lifetime.Kind(intent.ScopeKind), lifetime.Assurance(intent.Assurance), intent.ScopePlan)
	if err != nil {
		return nil, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, registration.Deadline)
	if err != nil {
		return nil, task.ErrEvidenceFault
	}
	bootID, err := lifetime.BootID()
	if err != nil || bootID != registration.BootID {
		return nil, errors.Join(task.ErrIdentityMismatch, err)
	}
	ref, err := lifetime.NewRef(lifetime.Kind(registration.ScopeKind), lifetime.Assurance(registration.Assurance), registration.ScopeID)
	if err != nil {
		return nil, err
	}
	scope, err := lifetime.OpenRegistered(plan, ref)
	if err != nil {
		return nil, err
	}
	conn, err := lifetimeConnectionFromFD(parsed.LifetimeFD)
	if err != nil {
		return nil, errors.Join(err, releaseLifetimeScopeUncanceled(context.Background(), scope))
	}
	runner, err := os.Executable()
	if err != nil {
		return nil, errors.Join(err, conn.Close(), releaseLifetimeScopeUncanceled(context.Background(), scope))
	}
	client, err := newLifetimeWorkerClient(conn, scope, parsed.TaskID, deadline, newRunnerLifetimeLaunchGateFactory(runner))
	if err != nil {
		return nil, errors.Join(err, conn.Close(), releaseLifetimeScopeUncanceled(context.Background(), scope))
	}
	return &openedLifetimeChild{control: client, deadline: deadline}, nil
}

func readLifetimeChildRecords(records lifetimeRegistrationReader) (*task.LifetimeRegistrationRecord, *task.LifetimeIntentRecord, error) {
	if records == nil {
		return nil, nil, task.ErrEvidenceFault
	}
	registration, err := records.ReadLifetimeRegistration()
	if err != nil {
		return nil, nil, err
	}
	if registration == nil {
		return nil, nil, task.ErrEvidenceFault
	}
	intent, err := records.ReadLifetimeIntent()
	if err != nil {
		return nil, nil, err
	}
	if intent == nil || !task.LifetimeIntentMatchesRegistration(intent, registration) {
		return nil, nil, task.ErrIdentityMismatch
	}
	return registration, intent, nil
}

func lifetimeConnectionFromFD(fd int) (net.Conn, error) {
	if fd < 3 {
		return nil, task.ErrEvidenceFault
	}
	file := os.NewFile(uintptr(fd), "delegate-lifetime-control")
	if file == nil {
		return nil, task.ErrEvidenceFault
	}
	conn, connErr := net.FileConn(file)
	closeErr := file.Close()
	if connErr != nil || closeErr != nil {
		if conn != nil {
			closeErr = errors.Join(closeErr, conn.Close())
		}
		return nil, errors.Join(connErr, closeErr)
	}
	return conn, nil
}

func (c *openedLifetimeChild) Lifetime() execution.Lifetime {
	if c == nil {
		return nil
	}
	return c.control
}

func (c *openedLifetimeChild) Deadline() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.deadline
}

func (c *openedLifetimeChild) Close() error {
	if c == nil || c.control == nil {
		return nil
	}
	return c.control.Close()
}
