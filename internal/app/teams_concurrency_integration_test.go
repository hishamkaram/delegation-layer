package app

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// Both callers resolve before either workspace owner is acquired. A barrier
// inside that exclusive owner would manufacture a deadlock, not a real race.
func TestTeamConcurrentDispatchKeepsCanonicalWinnerAndOneSubmission(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	resource, registration := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	recordClosedProvisionHelper(t, fixture, resource)
	path := filepath.Join(t.TempDir(), "catalog", "teams.sqlite")
	catalog, record, target := integrationTeam(t, path)
	fixture.deps.TeamCatalogPath = path
	fixture.deps.RunnerExecutable = canonicalAppTestExecutable(t)
	fixture.deps.Catalog = teamConcurrencyCatalog(t)
	configPath, statusPath, commandLog := teamConcurrencySupervisor(t, &fixture)
	fixture.deps = fixture.deps.normalized()
	options := dispatchOptions(fixture.deps, false)
	options, firstRead, bothRead, release, calls := synchronizedTeamReads(options)
	id := strings.Repeat("e", 32)
	arguments := Arguments{PueueConfig: configPath, Root: fixture.lineage.store.Root, TaskID: id, WorkspaceID: resource.Request().WorkspaceID, BriefBytes: []byte("brief"), TeamScope: "user", TeamID: target.Team.TeamID, MemberID: target.MemberID, Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"}}
	request := dispatchRequest(arguments, fixture.deps)
	results := make(chan tasklifecycle.DispatchResult, 2)
	var workers sync.WaitGroup
	var released sync.Once
	releaseReaders := func() { released.Do(func() { close(release) }) }
	t.Cleanup(func() { releaseReaders(); workers.Wait() })
	workers.Go(func() { results <- tasklifecycle.Dispatch(request, options) })
	select {
	case <-firstRead:
	case <-fixture.ctx().Done():
		t.Fatal("first catalog read did not complete")
	}
	updated := record.Definition.Clone()
	updated.Members[0].Name = "Updated primary"
	updated.Members[0].ExecutionDefaults.Model = "latest-model"
	record, _, err := catalog.Update(t.Context(), record.Reference, record.Revision, updated)
	if err != nil {
		t.Fatal(err)
	}
	workers.Go(func() { results <- tasklifecycle.Dispatch(request, options) })
	select {
	case <-bothRead:
	case <-fixture.ctx().Done():
		t.Fatal("second catalog read did not complete")
	}
	releaseReaders()
	one, two := <-results, <-results
	if one.Err() != nil && two.Err() != nil {
		t.Fatalf("neither concurrent caller admitted: %v / %v", one.Err(), two.Err())
	}
	td := assertConcurrentTeamWinner(t, fixture, resource, registration, id, record.Revision)
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerStatus(t, statusPath, fixture.lineage.store.RootID, id, receipt.NumericTaskID)
	if _, err = catalog.Delete(t.Context(), record.Reference, record.Revision); err != nil {
		t.Fatal(err)
	}
	// A busy loser may honestly return pending. Replaying the same caller after
	// the winner commits must use saved settings and never enqueue a second row.
	replay := tasklifecycle.Dispatch(request, dispatchOptions(fixture.deps, false))
	if replay.Err() != nil {
		t.Fatalf("replaying persisted winner: %v (first results %v / %v)", replay.Err(), one.Err(), two.Err())
	}
	assertOrdinarySubmissionCommandCount(t, commandLog, "add", 1)
	assertOrdinarySubmissionCommandCount(t, commandLog, "enqueue", 1)
	if calls.Load() != 2 {
		t.Fatalf("unexpected mutable lookups: %d", calls.Load())
	}
	if _, err = td.ReadSubmission(); err != nil {
		t.Fatal(err)
	}
}

func synchronizedTeamReads(options tasklifecycle.DispatchOptions) (tasklifecycle.DispatchOptions, <-chan struct{}, <-chan struct{}, chan<- struct{}, *atomic.Int32) {
	lookup := options.Members
	firstRead, bothRead, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	calls := &atomic.Int32{}
	options.Members = func(ctx context.Context, target task.TeamTarget, root, cwd, projectRoot string) (teamcontract.ResolvedMember, error) {
		selected, err := lookup(ctx, target, root, cwd, projectRoot)
		if err != nil {
			return selected, err
		}
		if calls.Add(1) == 1 {
			close(firstRead)
		} else {
			close(bothRead)
		}
		select {
		case <-release:
			return selected, nil
		case <-ctx.Done():
			return selected, ctx.Err()
		}
	}
	return options, firstRead, bothRead, release, calls
}

func assertConcurrentTeamWinner(t *testing.T, fixture checkpointRuntimeFixture, resource *workspace.Resource, registration workspace.Registration, id string, revision int64) *taskdir.TaskDir {
	t.Helper()
	td, err := fixture.lineage.store.OpenTask(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	winner, _, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	if winner.Team == nil || (winner.Team.TeamRevision != 1 && winner.Team.TeamRevision != revision) || winner.Workspace == nil {
		t.Fatalf("no canonical winner: %+v", winner)
	}
	expectedModel := ""
	if winner.Team.TeamRevision == revision {
		expectedModel = "latest-model"
	}
	if winner.RequestedConfig.Model != expectedModel {
		t.Fatalf("winner default and revision differ: %+v", winner)
	}
	subject, err := workspaces.TaskSubject(*winner)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := resource.CaptureObligation(t.Context(), winner.Workspace.Reservation)
	if err != nil || obligation.Subject != subject {
		t.Fatalf("capture did not bind canonical winner: %+v / %+v / %v", obligation.Subject, subject, err)
	}
	if winner.CanonicalCwd != registration.CanonicalCwd {
		t.Fatal("winner left managed checkout")
	}
	return td
}

func teamConcurrencySupervisor(t *testing.T, fixture *checkpointRuntimeFixture) (string, string, string) {
	t.Helper()
	supervisor, options, statusPath, commandLog := newOrdinarySubmissionReceiptSupervisor(t)
	t.Cleanup(func() {
		if err := supervisor.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.deps.SupervisorOptions = options
	fixture.deps.InitialSupervisorExecutable = supervisor.Binding().ClientExecutable
	return supervisor.Binding().ConfigPath, statusPath, commandLog
}

func teamConcurrencyCatalog(t *testing.T) commonprovider.Catalog {
	t.Helper()
	catalog := fixtureContinuationDependencies(t, nil, "").Catalog
	registration, err := catalog.Lookup("fixture:test")
	if err != nil {
		t.Fatal(err)
	}
	registration.Description.SupportedOptions = append(registration.Description.SupportedOptions, commonprovider.OptionModel)
	catalog, err = commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
