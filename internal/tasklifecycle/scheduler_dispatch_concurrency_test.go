package tasklifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const schedulerDispatchCallers = 4

// This bounded race case exercises Dispatch through production task creation.
// The typed validation-store hook then refuses submission deliberately: no
// queue row or provider runs, and this is not provider-completion coverage.
func TestSchedulerDispatchConcurrentCreationKeepsInvocationState(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(fmt.Sprintf("initialized=%t", initialized), func(t *testing.T) {
			schedulerDispatchCreationCase(t, initialized)
		})
	}
}

func schedulerDispatchCreationCase(t *testing.T, initialized bool) {
	t.Helper()
	base := supervisorfixture.CanonicalTempDir(t)
	root := filepath.Join(base, "state")
	if initialized {
		store, err := taskdir.InitStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if closeErr := store.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	// The watchdog releases fixture gates on a missing caller. It makes
	// no dispatch-latency claim; every goroutine is joined below.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	preparation := newSchedulerDispatchGate(ctx)
	persisted := newSchedulerDispatchGate(ctx)
	supervisor, configPath := schedulerDispatchSupervisor(t, base)
	var adds atomic.Int32
	requests := make([]DispatchRequest, schedulerDispatchCallers)
	options := make([]DispatchOptions, schedulerDispatchCallers)
	stops := make([]error, schedulerDispatchCallers)
	for index := range requests {
		requests[index] = schedulerDispatchRequest(t, root, configPath, index)
		stops[index] = fmt.Errorf("fixture submission boundary for caller %d", index)
		options[index] = schedulerDispatchOptions(requests[index], supervisor, preparation, persisted, stops[index], &adds)
	}
	results := schedulerDispatchConcurrently(requests, options)
	if preparation.remaining.Load() != 0 || persisted.remaining.Load() != 0 {
		t.Errorf("callers did not overlap both lifecycle boundaries: preparation=%d persisted=%d", preparation.remaining.Load(), persisted.remaining.Load())
	}
	for index, result := range results {
		schedulerDispatchAssertResult(t, requests[index], result, stops[index])
	}
	schedulerDispatchAssertStore(t, root, requests, results)
	if adds.Load() != 0 {
		t.Errorf("bounded creation reached supervisor add %d times", adds.Load())
	}
}

// Each gate owns its arrival count and closes ready once all four calls reach
// the real lifecycle boundary. The parent joins every caller before cleanup.
type schedulerDispatchGate struct {
	ctx       context.Context
	remaining atomic.Int32
	ready     chan struct{}
}

func newSchedulerDispatchGate(ctx context.Context) *schedulerDispatchGate {
	gate := &schedulerDispatchGate{ctx: ctx, ready: make(chan struct{})}
	gate.remaining.Store(schedulerDispatchCallers)
	return gate
}

func (gate *schedulerDispatchGate) wait() error {
	remaining := gate.remaining.Add(-1)
	if remaining < 0 {
		return errors.New("dispatch fixture boundary entered more than once per caller")
	}
	if remaining == 0 {
		close(gate.ready)
	}
	select {
	case <-gate.ready:
		return nil
	case <-gate.ctx.Done():
		return fmt.Errorf("dispatch fixture boundary missing %d callers: %w", gate.remaining.Load(), gate.ctx.Err())
	}
}

type schedulerDispatchCandidates struct {
	preparation *schedulerDispatchGate
	executable  string
}

func (source schedulerDispatchCandidates) Current(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	if err := source.preparation.wait(); err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	profile := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: source.executable, Directory: request.CanonicalCwd,
			Environment: slices.Clone(environment), Predicate: task.FixturePredicateRef(),
		},
		ObservedVersion: "scheduler-dispatch-fixture",
		Effective: task.EffectiveConfig{
			Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte(request.RequestedConfig.Model)),
		},
	}
	return commonprovider.ReadyCandidate(func(task.TaskRecord) (commonprovider.PreparedProfile, error) {
		return profile, nil
	})(request, environment)
}

func (source schedulerDispatchCandidates) ForTask(task.TaskRecord, *task.MetaRecord, []string) (commonprovider.ProfileCandidate, bool, error) {
	return commonprovider.ProfileCandidate{}, false, errors.New("fresh dispatch unexpectedly requested a historical candidate")
}

// The resource fixture has no mutable readings or native/provider probes. Each
// Dispatch still constructs and refreshes its own production resource gate.
type schedulerDispatchResources struct{}

func (schedulerDispatchResources) Check(context.Context, resourceadmission.TargetResolver) (resourceadmission.Assessment, error) {
	return resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, resourceadmission.Findings{}), nil
}

func (schedulerDispatchResources) Scope(context.Context, resourceadmission.TargetResolver) (resourceadmission.Scope, error) {
	return resourceadmission.Scope{}, nil
}

func schedulerDispatchOptions(request DispatchRequest, supervisor string, preparation, persisted *schedulerDispatchGate, stop error, adds *atomic.Int32) DispatchOptions {
	environment := []string{"SCHEDULER_DISPATCH_CALLER=" + request.TaskID}
	return DispatchOptions{
		OpenStore: func(root string, create bool) (*taskdir.Store, error) {
			if !create {
				return nil, errors.New("fresh dispatch did not request store initialization")
			}
			return taskdir.InitStore(root)
		},
		Resources: schedulerDispatchResources{},
		Creation: CreationOptions{
			PublisherVersion: "scheduler-dispatch-fixture",
			Admission: admissioncore.PreparationOptions{
				Profiles: admissioncore.NewProfiles(schedulerDispatchCandidates{preparation: preparation, executable: request.Selection.Runner}, nil, environment),
				Supervisor: supervisorcore.AdmissionOptions{
					InitialExecutable: supervisor, Environment: environment,
					Control: pueue.Options{Observer: func(event pueue.CommandEvent) {
						if event.Stage == "entry" && len(event.Argv) > 3 && event.Argv[3] == "add" {
							adds.Add(1)
						}
					}},
				},
				DescribeProvider: func(id string) (commonprovider.Description, error) {
					if id != task.FixturePredicateRef().Adapter {
						return commonprovider.Description{}, task.ErrIdentityMismatch
					}
					return commonprovider.Description{ID: id, SupportedModes: []string{"read-only"}}, nil
				},
			},
			OpenValidationStore: func(root string) (*taskdir.Store, error) {
				if root != request.Root {
					return nil, task.ErrIdentityMismatch
				}
				if err := persisted.wait(); err != nil {
					return nil, err
				}
				return nil, stop
			},
		},
	}
}

func schedulerDispatchRequest(t *testing.T, root, configPath string, index int) DispatchRequest {
	t.Helper()
	workspace := supervisorfixture.CanonicalTempDir(t)
	runner := filepath.Join(workspace, "inert-runner")
	supervisorfixture.WriteExecutable(t, runner, "#!/bin/sh\nexit 64\n")
	return DispatchRequest{
		Root: root, TaskID: strings.Repeat(string("abcd"[index]), 32),
		Provider: task.FixturePredicateRef().Adapter, Cwd: workspace,
		BriefBytes: []byte(fmt.Sprintf("independent dispatch brief %d\n", index)),
		Config: task.TaskConfig{
			Permission: "read-only", Budget: (time.Duration(index+1) * time.Minute).String(),
			Model: fmt.Sprintf("fixture-model-%d", index), Effort: fmt.Sprintf("fixture-effort-%d", index),
		},
		Selection: supervisorcore.AdmissionSelection{Runner: runner, ConfigPath: configPath},
	}
}

func schedulerDispatchConcurrently(requests []DispatchRequest, options []DispatchOptions) []DispatchResult {
	results := make([]DispatchResult, len(requests))
	var callers sync.WaitGroup
	for index := range requests {
		callers.Go(func() { results[index] = Dispatch(requests[index], options[index]) })
	}
	callers.Wait()
	return results
}

// Pueue's existing in-process control owner starts and waits for this finite
// fixture. It has no daemon, and accepts only the version probe needed before
// durable creation; it cannot add work or invoke the inert provider/runner.
func schedulerDispatchSupervisor(t *testing.T, base string) (string, string) {
	t.Helper()
	executable, configPath := filepath.Join(base, "pueue"), filepath.Join(base, "pueue.json")
	supervisorfixture.WriteExecutable(t, executable, `#!/bin/sh
set -eu
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  *) exit 64 ;;
esac
`)
	config := struct {
		Shared map[string]string `json:"shared"`
	}{Shared: map[string]string{}}
	for _, name := range []string{"pueue_directory", "runtime_directory", "unix_socket_path", "alias_file", "pid_path", "shared_secret_path", "daemon_cert", "daemon_key"} {
		config.Shared[name] = filepath.Join(base, name)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return executable, configPath
}

func schedulerDispatchAssertResult(t *testing.T, request DispatchRequest, result DispatchResult, stop error) {
	t.Helper()
	if result.TaskID != request.TaskID || result.Root != request.Root || result.Provider != request.Provider {
		t.Errorf("dispatch identity = %s/%s/%s, want %s/%s/%s", result.TaskID, result.Root, result.Provider, request.TaskID, request.Root, request.Provider)
	}
	if result.Existing != nil || result.RecordPresence() != RecordCreated || result.Creation == nil || result.Creation.Submission == nil {
		t.Errorf("caller %s did not reach fresh durable creation: %v", request.TaskID, result.Err())
		return
	}
	submission := result.Creation.Submission
	if submission.Stage != SubmissionRunnerValidation || !task.AllErrorCauses(result.Err(), func(cause error) bool { return errors.Is(cause, stop) }) {
		t.Errorf("caller %s did not settle at its own validation boundary: stage=%d err=%v", request.TaskID, submission.Stage, result.Err())
	}
	if result.StoreCloseError != nil || submission.TaskCloseError != nil || submission.PermitReleaseError != nil {
		t.Errorf("caller %s leaked closure faults: store=%v task=%v permit=%v", request.TaskID, result.StoreCloseError, submission.TaskCloseError, submission.PermitReleaseError)
	}
}

func schedulerDispatchAssertStore(t *testing.T, root string, requests []DispatchRequest, results []DispatchResult) {
	t.Helper()
	store, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	seen := make(map[string]bool, len(results))
	for index, result := range results {
		if seen[result.TaskID] {
			t.Errorf("duplicate task ID %s", result.TaskID)
		}
		seen[result.TaskID] = true
		if result.RootID != store.RootID {
			t.Errorf("caller %d used root ID %s, want %s", index, result.RootID, store.RootID)
		}
		schedulerDispatchAssertTask(t, store, requests[index])
	}
	entries, err := os.ReadDir(filepath.Join(root, "tasks"))
	if err != nil || len(entries) != len(requests) {
		t.Errorf("persisted task count=%d, want %d: %v", len(entries), len(requests), err)
	}
}

func schedulerDispatchAssertTask(t *testing.T, store *taskdir.Store, request DispatchRequest) {
	t.Helper()
	td, err := store.OpenTask(request.TaskID)
	if err != nil {
		t.Errorf("open persisted task %s: %v", request.TaskID, err)
		return
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	req, meta, err := td.PreparedRecords()
	if err != nil || req == nil || meta == nil {
		t.Errorf("read persisted task %s: %v", request.TaskID, err)
		return
	}
	schedulerDispatchAssertRequest(t, store, request, req, meta)
	schedulerDispatchAssertProfile(t, request, meta)
	brief, err := td.ReadBrief()
	if err != nil || !bytes.Equal(brief, request.BriefBytes) || req.BriefSHA256 != task.ComputeSHA256(request.BriefBytes) || req.BriefLength != int64(len(request.BriefBytes)) {
		t.Errorf("persisted brief crossed callers for %s: %v", request.TaskID, err)
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bounded creation unexpectedly prepared a submission for %s: %v", request.TaskID, err)
	}
}

func schedulerDispatchAssertRequest(t *testing.T, store *taskdir.Store, request DispatchRequest, req *task.TaskRecord, meta *task.MetaRecord) {
	t.Helper()
	if req.RootID != store.RootID || meta.RootID != store.RootID || req.TaskID != request.TaskID || meta.TaskID != request.TaskID || req.Provider != request.Provider || req.CanonicalCwd != request.Cwd {
		t.Errorf("persisted request identity crossed callers for %s", request.TaskID)
	}
	if !reflect.DeepEqual(req.RequestedConfig, request.Config) || !reflect.DeepEqual(meta.RequestedConfig, request.Config) {
		t.Errorf("persisted request/config crossed callers for %s: request=%+v meta=%+v want=%+v", request.TaskID, req.RequestedConfig, meta.RequestedConfig, request.Config)
	}
}

func schedulerDispatchAssertProfile(t *testing.T, request DispatchRequest, meta *task.MetaRecord) {
	t.Helper()
	if !meta.EnvironmentRecorded || !slices.Equal(meta.Environment, []string{"SCHEDULER_DISPATCH_CALLER=" + request.TaskID}) || meta.EffectiveConfig.Digest != task.ComputeSHA256([]byte(request.Config.Model)) || meta.RunnerExecutable != request.Selection.Runner {
		t.Errorf("persisted profile crossed callers for %s", request.TaskID)
	}
}
