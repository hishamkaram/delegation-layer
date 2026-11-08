package tasklifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ResourceChecker is the consumer-owned port for the resource check of one
// fresh ordinary admission. resourceadmission.Checker implements it; a nil
// option selects that native checker when a fresh admission first needs it.
type ResourceChecker interface {
	Check(ctx context.Context, resolver resourceadmission.TargetResolver) (resourceadmission.Assessment, error)
	Scope(ctx context.Context, resolver resourceadmission.TargetResolver) (resourceadmission.Scope, error)
}

// ErrResourcePressure marks a fresh admission refused because resource
// pressure was confirmed. ResourcePressureRefusal is the classifier.
var ErrResourcePressure = errors.New("resource pressure refused new work")

// ResourcePressureError carries the confirmed pressure behind a refusal.
type ResourcePressureError struct {
	pressure resourceadmission.Pressure
}

func (e *ResourcePressureError) Error() string {
	return fmt.Sprintf("%v: %s %s", ErrResourcePressure, e.pressure.Metric, e.pressure.Basis)
}

func (e *ResourcePressureError) Unwrap() error { return ErrResourcePressure }

// ResourcePressureRefusal reports the refused pressure only when every cause
// of err is that refusal. A refusal joined with a close, cleanup, or any
// other fault is an ordinary failure, so it never inherits retry guidance.
func ResourcePressureRefusal(err error) (resourceadmission.Pressure, bool) {
	if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrResourcePressure) }) {
		return resourceadmission.Pressure{}, false
	}
	var refusal *ResourcePressureError
	if !errors.As(err, &refusal) {
		return resourceadmission.Pressure{}, false
	}
	return refusal.pressure, true
}

func resourceRefusal(assessment resourceadmission.Assessment) error {
	if assessment.Status() != resourceadmission.StatusPressure {
		return nil
	}
	return &ResourcePressureError{pressure: assessment.Pressures()[0]}
}

func (options DispatchOptions) resourceChecker() ResourceChecker {
	if options.Resources != nil {
		return options.Resources
	}
	return resourceadmission.NewChecker(options.ResourceGuards)
}

// executionTargetResolver names the process that would run the admitted task:
// the verified private daemon, or this process when no private daemon exists
// yet and admission would start one that inherits this process's scope.
type executionTargetResolver struct {
	selection supervisorcore.AdmissionSelection
	root      string
	runtime   supervisorcore.AdmissionOptions
}

func (r executionTargetResolver) ResolveTarget(ctx context.Context) (resourceadmission.Target, error) {
	target, err := supervisorcore.ObserveAdmissionExecutionTarget(ctx, r.selection, r.root, r.runtime)
	if err != nil {
		return resourceadmission.Target{}, err
	}
	switch target.State() {
	case pueue.ExecutionTargetRunning:
		return resourceadmission.Target{Identity: fmt.Sprintf("supervisor:%d:%s", target.PID(), target.StartToken()), PID: target.PID()}, nil
	case pueue.ExecutionTargetAbsent:
		pid := os.Getpid()
		return resourceadmission.Target{Identity: fmt.Sprintf("caller:%d", pid), PID: pid}, nil
	case pueue.ExecutionTargetUnknown:
	}
	return resourceadmission.Target{}, pueue.ErrExecutionTargetUnverified
}

// resourceGate owns the invocation-local assessment of one task ID that was
// authoritatively absent when its fresh admission began. It is never
// persisted and never consulted for an existing or partial task.
type resourceGate struct {
	checker    ResourceChecker
	policy     resourceadmission.GuardPolicy
	store      *taskdir.Store
	taskID     string
	assessment resourceadmission.Assessment
	observed   bool
}

// taskEntryAbsent is true only for a definite absence observation. A present,
// partial, or unreadable entry belongs to the existing-task recovery path.
func taskEntryAbsent(store *taskdir.Store, id string) bool {
	exists, err := store.TaskEntryExists(id)
	return err == nil && !exists
}

// openResourceGate checks a fresh task ID before workspace and provider
// preparation. It returns no gate for an ID that is not definitely absent,
// and reuses an assessment already taken by the continuation that owns this
// dispatch instead of sampling again.
func openResourceGate(request DispatchRequest, options DispatchOptions, store *taskdir.Store, id string) (*resourceGate, error) {
	return openResourceGateContext(context.Background(), request, options, store, id)
}

func openResourceGateContext(ctx context.Context, request DispatchRequest, options DispatchOptions, store *taskdir.Store, id string) (*resourceGate, error) {
	if !taskEntryAbsent(store, id) {
		return nil, nil
	}
	gate := &resourceGate{checker: options.resourceChecker(), policy: options.ResourceGuards, store: store, taskID: id}
	if options.resourceAssessment != nil {
		gate.assessment, gate.observed = *options.resourceAssessment, true
		return gate, gate.refusal()
	}
	resolver := executionTargetResolver{selection: request.Selection, root: store.Root, runtime: options.Creation.Admission.Supervisor}
	if err := gate.checkContext(ctx, resolver); err != nil {
		return gate, err
	}
	return gate, gate.refusal()
}

func (g *resourceGate) checkContext(ctx context.Context, resolver resourceadmission.TargetResolver) error {
	assessment, err := g.checker.Check(ctx, resolver)
	if err != nil {
		return err
	}
	g.assessment, g.observed = assessment, true
	return nil
}

// refusal refuses confirmed pressure only while the task entry is still
// definitely absent. An entry that appeared meanwhile keeps its existing flow.
func (g *resourceGate) refusal() error {
	err := resourceRefusal(g.assessment)
	if err == nil || !taskEntryAbsent(g.store, g.taskID) {
		return nil
	}
	return err
}

// refresh re-checks the actual prepared binding just before the fresh task's
// records are written, when the earlier assessment is stale or measured a
// different execution target. An entry that appeared during preparation
// belongs to existing-task recovery, so it takes no further resource reading
// and keeps the create-once flow unchanged.
func (g *resourceGate) refresh(binding task.SupervisorRef, runtime supervisorcore.AdmissionOptions) error {
	return g.refreshContext(context.Background(), binding, runtime)
}

func (g *resourceGate) refreshContext(ctx context.Context, binding task.SupervisorRef, runtime supervisorcore.AdmissionOptions) error {
	if g == nil || !taskEntryAbsent(g.store, g.taskID) {
		return nil
	}
	resolver := executionTargetResolver{selection: supervisorcore.AdmissionSelection{SavedSupervisor: &binding}, root: g.store.Root, runtime: runtime}
	scope, err := g.checker.Scope(ctx, resolver)
	if err != nil {
		return err
	}
	if g.observed && g.assessment.Policy() == g.policy && g.assessment.Reusable(time.Now(), scope) {
		return nil
	}
	if err = g.checkContext(ctx, resolver); err != nil {
		return err
	}
	return g.refusal()
}

// current is the latest assessment for projection, or nil when none exists.
func (g *resourceGate) current() *resourceadmission.Assessment {
	if g == nil || !g.observed {
		return nil
	}
	assessment := g.assessment
	return &assessment
}

// checkContinuationResources takes the one initial assessment of a
// continuation after its predecessor is loaded and before any successor
// workspace, supervisor, or provider work. The successor ID is generated
// later by this continuation, so no task entry can exist for it yet.
func checkContinuationResources(ctx context.Context, request DispatchRequest, root string, options DispatchOptions) (*resourceadmission.Assessment, error) {
	resolver := executionTargetResolver{selection: request.Selection, root: root, runtime: options.Creation.Admission.Supervisor}
	assessment, err := options.resourceChecker().Check(ctx, resolver)
	if err != nil {
		return nil, err
	}
	return &assessment, resourceRefusal(assessment)
}
