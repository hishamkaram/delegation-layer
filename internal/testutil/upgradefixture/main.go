// Command upgradefixture seeds one owned cooperative inspection whose real
// worker was observed and Waited, but whose journal intentionally lacks closure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type fixtureScope struct {
	ctx    context.Context
	native *lifetime.Scope
}

func (s fixtureScope) Context() context.Context { return s.ctx }
func (s fixtureScope) StartCommand(cmd *exec.Cmd, start func() error) error {
	if err := s.native.Configure(cmd); err != nil {
		return err
	}
	return start()
}
func (fixtureScope) FinishCommand(*exec.Cmd) error { return nil }

type proof struct {
	OperationID string `json:"operation_id"`
	AdmissionID string `json:"admission_id"`
	RootID      string `json:"root_id"`
	Observed    bool   `json:"worker_observed_before_wait"`
	Waited      bool   `json:"worker_waited"`
	ScopeEmpty  bool   `json:"native_scope_empty"`
}

func seed(ctx context.Context, root, workspace, id, admissionID string, admissionOnly bool) (result proof, resultErr error) {
	if !admissionOnly && runtime.GOOS != "darwin" {
		return result, errors.New("native cooperative settlement fixture requires macOS")
	}
	if err := task.ValidateTaskID(id); err != nil {
		return result, err
	}
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	resultErr = pueue.WithExistingBootstrapLockContext(ctx, root, func(locked context.Context) error {
		if seedErr := seedAdmission(locked, store, workspace, admissionID); seedErr != nil {
			return seedErr
		}
		if admissionOnly {
			result = proof{RootID: store.RootID, AdmissionID: admissionID}
			return nil
		}
		var seedErr error
		result, seedErr = seedLocked(locked, store, workspace, id)
		result.AdmissionID = admissionID
		return seedErr
	})
	return result, resultErr
}

func seedLocked(ctx context.Context, store *taskdir.Store, workspace, id string) (result proof, resultErr error) {
	binding, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found {
		return result, errors.Join(errors.New("fixture requires an existing private supervisor binding"), err)
	}
	worker, err := exec.LookPath("sleep")
	if err != nil {
		return result, err
	}
	helper, err := os.Executable()
	if err != nil {
		return result, err
	}
	op, err := openFixtureInspection(ctx, store, workspace, id, helper, worker, binding)
	if err != nil {
		return result, fmt.Errorf("construct cooperative fixture journal: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, op.Close()) }()
	owner, err := op.AcquireLifetimeOwner(ctx)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	native, boot, err := createFixtureScope(ctx, store.RootID, id, op)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, native.DestroyEmpty(), native.Release()) }()
	var observed lifetime.LaunchRef
	// A finite one-second child leaves a native observation window before Wait.
	cmd := exec.Command(worker, "1")
	cmd.Dir = workspace
	cmd.Env = []string{}
	err = execution.RunScopedCommand(fixtureScope{ctx: ctx, native: native}, cmd, execution.CommandHooks{
		NullOutput: true,
		Started: func() error {
			var observeErr error
			observed, observeErr = native.ObserveStarted(cmd.Process.Pid)
			if observeErr != nil {
				return observeErr
			}
			_, observeErr = op.RecordLifetimeWorkerContext(ctx, taskdir.LifetimeWorkerFacts{
				PID: observed.PID(), ProcessStart: observed.StartToken(), BootID: boot,
			})
			return observeErr
		},
	})
	if err != nil {
		return result, err
	}
	empty, err := native.EmptyContext(ctx, []lifetime.LaunchRef{observed})
	if err != nil || !empty {
		return result, errors.Join(errors.New("owned finite worker scope did not close"), err)
	}
	// Deliberately omit journal worker-Wait/termination to reproduce the durable
	// cooperative owner-loss gap. The test creator retains independent native
	// Start/Wait and empty-scope evidence; this is never provider execution.
	return proof{OperationID: id, RootID: store.RootID, Observed: true, Waited: true, ScopeEmpty: true}, nil
}

func run() error {
	root := flag.String("root", "", "owned existing fixture root")
	workspace := flag.String("workspace", "", "owned fixture workspace")
	id := flag.String("id", "", "fresh synthetic inspection ID")
	admissionID := flag.String("admission-id", "", "fresh synthetic receiptless task ID")
	admissionOnly := flag.Bool("admission-only", false, "seed only admission evidence on a non-cooperative backend")
	observeTask := flag.String("observe-task-closure", "", "read only this task's canonical execution closure")
	flag.Parse()
	if *root == "" {
		return errors.New("root is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if *observeTask != "" {
		if *workspace != "" || *id != "" || *admissionID != "" || *admissionOnly {
			return errors.New("task closure observation cannot seed fixture state")
		}
		return encodeTaskClosure(ctx, *root, *observeTask)
	}
	if *workspace == "" {
		return errors.New("workspace is required for fixture seeding")
	}
	result, err := seed(ctx, *root, *workspace, *id, *admissionID, *admissionOnly)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func createFixtureScope(ctx context.Context, rootID, id string, op *inspection.Operation) (*lifetime.Scope, string, error) {
	plan, err := lifetime.Prepare(rootID, task.LifetimeOwnerInspection, id)
	if err != nil {
		return nil, "", err
	}
	if plan.Kind() != lifetime.KindProcessGroup || plan.Assurance() != lifetime.AssuranceCooperative {
		return nil, "", errors.New("fixture selected a non-cooperative native backend")
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return nil, "", err
	}
	if _, err = op.RecordLifetimeIntentContext(ctx, taskdir.LifetimeIntentFacts{
		ScopeKind: string(plan.Kind()), Assurance: string(plan.Assurance()), ScopePlan: plan.Identifier(),
		BootID: boot, Deadline: op.Deadline(),
	}); err != nil {
		return nil, "", err
	}
	native, err := lifetime.CreatePreparedContext(ctx, plan)
	if err != nil {
		return nil, "", err
	}
	// Match Guardian.bindAndRecordRunScope: cooperative emptiness requires
	// association with this actual execution owner before registration/Start.
	if err = native.BindOwnerProcess(); err != nil {
		return nil, "", errors.Join(fmt.Errorf("bind fixture scope owner: %w", err), native.DestroyEmpty(), native.Release())
	}
	ref := native.Reference()
	if _, err = op.RecordLifetimeRegistrationContext(ctx, taskdir.LifetimeRegistrationFacts{
		ScopeKind: string(ref.Kind()), Assurance: string(ref.Assurance()), ScopeID: ref.Identifier(),
		BootID: boot, Deadline: op.Deadline(),
	}); err != nil {
		return nil, "", errors.Join(err, native.DestroyEmpty(), native.Release())
	}
	return native, boot, nil
}
