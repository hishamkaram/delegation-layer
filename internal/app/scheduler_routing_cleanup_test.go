package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type schedulerRetainedRouting struct {
	infrastructureOwnerRouting
	store  *taskdir.Store
	source infrastructure.SavedBindingSource
	calls  int
}

func (r *schedulerRetainedRouting) RecoverSaved(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	if err := ctx.Err(); err != nil {
		return infrastructure.Disposition{}, err
	}
	if _, finite := ctx.Deadline(); !finite {
		return infrastructure.Disposition{}, os.ErrInvalid
	}
	if selected.Root != store.Root || selected.RootID != store.RootID || source.RootID != store.RootID {
		return infrastructure.Disposition{}, infrastructure.ErrIdentity
	}
	r.store, r.source = store, source
	r.calls++
	return infrastructure.Disposition{}, &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: store}, cause: os.ErrPermission}
}

func (r *schedulerRetainedRouting) EnsurePrivate(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, error) {
	if err := ctx.Err(); err != nil {
		return infrastructure.Disposition{}, err
	}
	if selected.Root != store.Root || selected.RootID != store.RootID {
		return infrastructure.Disposition{}, infrastructure.ErrIdentity
	}
	r.store = store
	r.calls++
	return infrastructure.Disposition{}, &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: store}, cause: os.ErrPermission}
}

func TestSchedulerDispatchRetainsCleanupBeforeAnyObservation(t *testing.T) {
	fixture := newTeamsScheduleFixture(t)
	fixture.create(t)
	fixture.persistTasks(t)
	binding := schedulerPrivateSavedBinding(t, fixture.state)
	tasks := fixture.tasks()
	tasks.arguments.PueueConfig = ""
	tasks.dependencies.Environment = []string{}
	tasks.dependencies.InitialSupervisorExecutable = binding.ClientExecutable
	routing := &schedulerRetainedRouting{}
	tasks.routing = routing
	id := strings.Repeat("b", 32)
	definition := fixture.claim(t, id)
	t.Cleanup(func() { closeSchedulerRoutingTestResources(t, routing.store) })
	state, dispatchErr := tasks.Dispatch(schedulerruntime.DispatchRequest{TaskID: id, Definition: definition})
	if routing.calls != 1 || routing.store == nil {
		t.Fatalf("real dispatch did not reach independent preparation: calls=%d error=%v", routing.calls, dispatchErr)
	}
	if !state.Observation.ObservedAt.IsZero() || state.Release != nil {
		t.Fatalf("scheduler observed again after unresolved dispatch cleanup: %+v", state)
	}
	var retained *taskAdmissionCleanupError
	if !errors.Is(dispatchErr, schedulerruntime.ErrTaskCleanupUnresolved) || !errors.As(dispatchErr, &retained) || retained == nil || retained.store != routing.store {
		t.Fatalf("dispatch lost its actual retained parent or Store: %v", dispatchErr)
	}
	if _, pinErr := routing.store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("dispatch closed its retained Store: %v", pinErr)
	}
	if _, statErr := os.Lstat(fixture.providerMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused dispatch launched provider: %v", statErr)
	}
}

func TestSchedulerObserveRetainsActualRoutedStoreAndStopsAdmission(t *testing.T) {
	store, err := taskdir.InitStore(supervisorfixture.CanonicalTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSchedulerRoutingTestResources(t, store) })
	template, original := newAppTaskInStore(t, store, strings.Repeat("c", 32), false, nil)
	t.Cleanup(func() {
		if closeErr := template.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	binding := schedulerPrivateSavedBinding(t, store)
	_, meta, err := template.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID = strings.Repeat("a", 32)
	meta.TaskID, meta.SupervisorConfig = request.TaskID, binding
	td, err := store.CreateTask(request.TaskID, &request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit, err := td.PrepareSubmission(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
		t.Fatal(err)
	}
	recordSchedulerFixtureReceipt(t, td)
	routing := &schedulerRetainedRouting{}
	t.Cleanup(func() { closeSchedulerRoutingTestResources(t, routing.store) })
	tasks := schedulerObservationFixture(store)
	tasks.dependencies.InitialSupervisorExecutable = binding.ClientExecutable
	tasks.routing = routing
	_, observationErr := tasks.Observe(td.TaskID)
	if routing.calls != 1 || routing.store == nil || routing.store == store || routing.source.Kind != infrastructure.SavedBindingTaskSubmission {
		t.Fatalf("real scheduler Status path did not route the committed private source: calls=%d source=%+v error=%v", routing.calls, routing.source, observationErr)
	}
	if _, pinErr := routing.store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("actual Observe closed the routed borrowed Store: %v", pinErr)
	}
	if !errors.Is(observationErr, schedulerruntime.ErrTaskCleanupUnresolved) || !errors.Is(observationErr, os.ErrPermission) {
		t.Fatalf("actual observation lost fatal cleanup and original cause: %v", observationErr)
	}
}

func closeSchedulerRoutingTestResources(t *testing.T, store *taskdir.Store) {
	t.Helper()
	if store != nil {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}
}

// Required config and saved binding are owned fixture data. Derived data/run
// directories are absent, so the real read-only probe needs routing. No daemon
// or provider is launched; the recording port returns the actual borrowed Store.
func schedulerPrivateSavedBinding(t *testing.T, store *taskdir.Store) task.SupervisorRef {
	t.Helper()
	writeSchedulerPrivateSavedConfig(t, store.Root)
	bundle := supervisorfixture.CanonicalTempDir(t)
	clientPath, daemonPath := filepath.Join(bundle, "pueue"), filepath.Join(bundle, "pueued")
	const clientBody = "#!/bin/sh\nprintf 'pueue 4.0.4\\n'\n"
	const daemonBody = "#!/bin/sh\nexit 99\n"
	supervisorfixture.WriteExecutable(t, clientPath, clientBody)
	supervisorfixture.WriteExecutable(t, daemonPath, daemonBody)
	client, err := pueue.Bind(t.Context(), clientPath, pueue.PrivateConfigPath(store.Root), pueue.Options{Environment: []string{}})
	if client != nil {
		t.Cleanup(func() {
			if closeErr := client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	binding := client.Binding()
	binding.DaemonExecutable = daemonPath
	if err = task.ValidateFreshSupervisorRef(binding); err != nil {
		t.Fatal(err)
	}
	return binding
}

func writeSchedulerPrivateSavedConfig(t *testing.T, root string) {
	t.Helper()
	base := filepath.Dir(pueue.PrivateConfigPath(root))
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	type privateShared struct {
		PueueDirectory   string `json:"pueue_directory"`
		RuntimeDirectory string `json:"runtime_directory"`
		UnixSocketPath   string `json:"unix_socket_path"`
		AliasFile        string `json:"alias_file"`
		PIDPath          string `json:"pid_path"`
		SharedSecretPath string `json:"shared_secret_path"`
		DaemonCert       string `json:"daemon_cert"`
		DaemonKey        string `json:"daemon_key"`
		Permissions      uint32 `json:"unix_socket_permissions"`
	}
	config, err := task.MarshalCanonical(struct {
		Shared privateShared `json:"shared"`
		Daemon struct {
			ShellCommand []string `json:"shell_command"`
		} `json:"daemon"`
	}{Shared: privateShared{PueueDirectory: filepath.Join(base, "data"), RuntimeDirectory: filepath.Join(base, "run"), UnixSocketPath: filepath.Join(base, "socket"), AliasFile: filepath.Join(base, "aliases.yml"), PIDPath: filepath.Join(base, "run", "pueue.pid"), SharedSecretPath: filepath.Join(base, "data", "shared_secret"), DaemonCert: filepath.Join(base, "data", "certs", "daemon.cert"), DaemonKey: filepath.Join(base, "data", "certs", "daemon.key"), Permissions: 0o700}, Daemon: struct {
		ShellCommand []string `json:"shell_command"`
	}{[]string{"/bin/sh", "-c", "{{ pueue_command_string }}"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pueue.PrivateConfigPath(root), config, 0o600); err != nil {
		t.Fatal(err)
	}
}
