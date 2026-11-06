package workspaces

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// OperationRuntime supplies application-composed runner and supervisor edges.
// The coordinator owns deadlines, durable purposes, permits, and every acquired
// client, including a client returned with an error while a probe is pending.
type OperationRuntime interface {
	ManagedRunner() (string, error)
	PrepareManaged(context.Context, *taskdir.Store, string) (string, *pueue.Client, error)
	OpenSaved(context.Context, *taskdir.Store, task.SupervisorRef, infrastructure.SavedBindingSource) (*pueue.Client, error)
}

// InspectionRecovery applies admission's recovery policy to the inspection
// selected and validated by the workspace coordinator. Admission owns recovery
// leases; the coordinator owns whether the reservation may close. A nil port
// is an evidence fault when an existing inspection requires recovery.
type InspectionRecovery func(context.Context, *inspection.Operation) error

// OperationOptions carries immutable invocation configuration. The canonical
// supervisor timeout policy interprets ObservationTimeout, including its default.
// Environment is the launch scope captured by composition. Git admission
// checks validate it and a verification job receives it; nil is a missing
// capture and fails instead of reading the process environment.
type OperationOptions struct {
	Runtime            OperationRuntime
	ObservationTimeout time.Duration
	RecoverInspection  InspectionRecovery
	Environment        []string
}

// ValidateGitEnvironment rejects inherited Git redirections in the captured
// launch scope before a caller authorizes any Git operation.
func (options OperationOptions) ValidateGitEnvironment() error {
	environment, err := options.scopeEnvironment()
	if err != nil {
		return err
	}
	return (workspace.Git{Environment: environment}).ValidateEnvironment()
}

// scopeEnvironment returns a copy of the captured launch scope.
func (options OperationOptions) scopeEnvironment() ([]string, error) {
	if options.Environment == nil {
		return nil, fmt.Errorf("%w: workspace launch environment was not captured", task.ErrEvidenceFault)
	}
	return slices.Clone(options.Environment), nil
}
