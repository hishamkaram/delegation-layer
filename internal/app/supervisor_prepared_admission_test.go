package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDispatchDuringPreparedMigrationRefusesBeforeTaskCreation(t *testing.T) {
	store, deps, legacy, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	control, cancelControl := supervisorcore.ControlContextWithParent(t.Context(), deps.SupervisorOptions)
	defer cancelControl()
	warm, err := supervisorcore.PreparePrivateAdmissionSupervisorContext(control, deps.supervisorLookup(), store, componentPublicationOptions(t, store.Root, owner, deps.SupervisorOptions))
	if err != nil || warm == nil {
		t.Fatalf("prepare complete admission fixture: client=%v err=%v", warm, err)
	}
	if err = warm.Close(); err != nil {
		t.Fatal(err)
	}
	prepareBusyAdmissionMigration(t, store.Root, legacy.Binding(), deps.SupervisorOptions, owner)
	journalPath := filepath.Join(store.Root, ".supervisor", "migration.json")
	journalBefore := readSupervisorProbeTestFile(t, journalPath)
	provider := preflightFixture{root: store.Root, cwd: testsupport.CanonicalTempDir(t)}
	deps.Catalog = preflightFixtureCatalog(t, provider.candidate)
	deps.RunnerExecutable = canonicalAppTestExecutable(t)
	deps.PublisherVersion = "prepared-admission-test"
	brief := filepath.Join(provider.cwd, "brief.md")
	if err = os.WriteFile(brief, []byte("must not create an ordinary task while migration is prepared"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--root", store.Root, "dispatch", "--provider", "fixture:preflight",
		"--cwd", provider.cwd, "--brief", brief, "--runner", deps.RunnerExecutable,
		"--permission", "read-only", "--budget", "1m", "--json",
	}, &stdout, &stderr, deps)
	var response Response
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode dispatch response: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
	}
	if code != 2 || response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Code != "supervisor_upgrade_required" {
		t.Errorf("prepared migration lost pre-admission guidance: code=%d response=%s", code, &stdout)
	}
	if response.Failure != nil && (response.Failure.Stage != "start-supervisor" || response.Failure.NextAction != "run_supervisor_migrate") {
		t.Errorf("prepared migration lost its recovery command: %+v", response.Failure)
	}
	if response.TaskID == "" {
		t.Fatal("dispatch did not identify the refused request")
	}
	if _, statErr := os.Stat(filepath.Join(store.Root, "tasks", response.TaskID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("prepared migration created an ordinary task: %v", statErr)
	}
	assertSupervisorProbeTestFile(t, journalPath, journalBefore)
}

func prepareBusyAdmissionMigration(t *testing.T, root string, binding task.SupervisorRef, options pueue.Options, owner *componentSupervisorFixture) {
	t.Helper()
	options = componentPublicationOptions(t, root, owner, options)
	path := filepath.Join(testsupport.CanonicalTempDir(t), "next-pueued")
	writeSupervisorProbeDaemon(t, path)
	options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []pueue.Job) error {
		return pueue.ErrPrivateSupervisorMigrationBusy
	}
	err := pueue.WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		successor, prepareErr := pueue.PreparePrivateForMigrationWithLock(ctx, binding.ClientExecutable, path, root, options)
		if prepareErr != nil {
			return errors.Join(prepareErr, successor.CloseAfterPending(prepareErr))
		}
		migrationErr := successor.MigratePrivateSupervisorWithLock(ctx, root)
		return errors.Join(migrationErr, successor.CloseAfterPending(migrationErr))
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("fixture did not retain a prepared migration: %v", err)
	}
	if !strings.HasPrefix(binding.Endpoint, "unix:") {
		t.Fatal("admission fixture must retain a live private endpoint")
	}
}
