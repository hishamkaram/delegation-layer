package testsupport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func NewTerminalEventInspectionSupervisor(t *testing.T, store *taskdir.Store, evidence supervisorcore.InspectionEvidence) (*pueue.Client, *atomic.Int32) {
	client, statusCalls, _ := NewTerminalEventInspectionSupervisorWithEnvironment(t, store, evidence)
	return client, statusCalls
}

func NewTerminalEventInspectionSupervisorWithEnvironment(t *testing.T, store *taskdir.Store, evidence supervisorcore.InspectionEvidence, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return NewTerminalEventInspectionSupervisorWithLauncher(t, store, evidence, nil, extraEnvironment...)
}

func NewTerminalEventInspectionSupervisorWithLauncher(t *testing.T, store *taskdir.Store, evidence supervisorcore.InspectionEvidence, launcher func(context.Context, string, string) error, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return NewTerminalEventInspectionSupervisorWithObserver(t, store, evidence, launcher, nil, extraEnvironment...)
}

func NewTerminalEventInspectionSupervisorWithObserver(t *testing.T, store *taskdir.Store, evidence supervisorcore.InspectionEvidence, launcher func(context.Context, string, string) error, observer func(pueue.CommandEvent), extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	cleanup := supervisorcore.NewCleanup(evidence, workspacescore.InspectPrivateOperation)
	bundle := supervisorfixture.CanonicalTempDir(t)
	clientPath := filepath.Join(bundle, "pueue")
	daemonPath := filepath.Join(bundle, "pueued")
	WriteTerminalEventInspectionSupervisorExecutables(t, clientPath, daemonPath)
	var statusCalls atomic.Int32
	var unfilteredStatusCalls atomic.Int32
	options := pueue.Options{
		ObservationTimeout:              inspection.AdmissionTimeout,
		SuccessfulCleanupAuthorizer:     cleanup.AuthorizeSuccessfulCleanup,
		PrivateTerminalEvidencePreparer: cleanup.PrepareTerminalEvidence,
		PrivateMaintenanceLauncher:      launcher,
		Environment:                     append(os.Environ(), extraEnvironment...),
		Observer: func(event pueue.CommandEvent) {
			if event.Stage == "entry" && slices.Contains(event.Argv, "status") {
				statusCalls.Add(1)
				if !slices.ContainsFunc(event.Argv, TerminalMaintenanceLabelFilter) {
					unfilteredStatusCalls.Add(1)
				}
			}
			if observer != nil {
				observer(event)
			}
		},
	}
	bootstrap, err := pueue.BindPrivate(t.Context(), clientPath, daemonPath, store.Root, options)
	if err != nil {
		t.Fatal(err)
	}
	ListenSupervisorProbeTestEndpoint(t, bootstrap.Binding().Endpoint)
	client, err := supervisorcore.PreparePrivateAdmissionSupervisor(clientPath, store, options)
	SettleSupervisorProbeTestClient(t, client, err)
	if closeErr := bootstrap.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	statusCalls.Store(0)
	unfilteredStatusCalls.Store(0)
	for _, name := range []string{"terminal-event-cleanup-blocked", "terminal-event-removals"} {
		if removeErr := os.Remove(filepath.Join(store.Root, ".supervisor", name)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Fatal(removeErr)
		}
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client, &statusCalls, &unfilteredStatusCalls
}

func NewTerminalInspectionCleanupSupervisor(t *testing.T, store *taskdir.Store, evidence supervisorcore.InspectionEvidence) (*pueue.Client, string, string) {
	t.Helper()
	cleanup := supervisorcore.NewCleanup(evidence, workspacescore.InspectPrivateOperation)
	base := filepath.Join(store.Root, ".supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "cleanup-pueue")
	statusPath := filepath.Join(base, "cleanup-status.json")
	removals := filepath.Join(base, "cleanup-removals")
	statusQueries := filepath.Join(base, "cleanup-status-queries")
	script := `#!/bin/sh
set -eu
command=''
last=''
for arg in "$@"; do
  last="$arg"
  case "$arg" in
    --version|status|remove|add) command="$arg" ;;
  esac
done
case "$command" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status) printf '%s\n' "${5-}" >> "$FAKE_STATUS_QUERIES"; cat "$FAKE_STATUS" ;;
  remove) printf '%s\n' "$last" >> "$FAKE_REMOVALS" ;;
  add) printf '%s\n' '9' ;;
  *) exit 64 ;;
esac
`
	if err := os.WriteFile(clientPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := pueue.PrivateConfigPath(store.Root)
	config, err := json.Marshal(map[string]any{
		"shared": map[string]any{
			"pueue_directory":    filepath.Join(base, "data"),
			"runtime_directory":  filepath.Join(base, "run"),
			"unix_socket_path":   filepath.Join(base, "socket"),
			"alias_file":         filepath.Join(base, "aliases"),
			"pid_path":           filepath.Join(base, "pid"),
			"shared_secret_path": filepath.Join(base, "secret"),
			"daemon_cert":        filepath.Join(base, "cert"),
			"daemon_key":         filepath.Join(base, "key"),
		},
		"daemon": map[string]any{
			"shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = pueue.WithBootstrapLock(t.Context(), store.Root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	options := pueue.Options{
		Environment:                     append(os.Environ(), "FAKE_STATUS="+statusPath, "FAKE_REMOVALS="+removals, "FAKE_STATUS_QUERIES="+statusQueries),
		ObservationTimeout:              5 * time.Second,
		SuccessfulCleanupAuthorizer:     cleanup.AuthorizeSuccessfulCleanup,
		PrivateTerminalEvidencePreparer: cleanup.PrepareTerminalEvidence,
	}
	client, err := pueue.Bind(t.Context(), clientPath, configPath, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client, removals, statusQueries
}
