package app

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	schedulecontract "github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/teams"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
	"modernc.org/sqlite"
)

var errTeamsScheduleRefusal = errors.New("finite schedule provider preparation refusal")

// SQLite owns persisted state. This fixture records only calls at the real
// provider preparation port; a refusal is never counted as provider execution.
type teamsScheduleFixture struct {
	state          *taskdir.Store
	deps           Dependencies
	catalog        *teams.Catalog
	ref            task.TeamReference
	catalogID      string
	arguments      Arguments
	store          schedulecontract.Store
	mu             sync.Mutex
	requests       []task.TaskRecord
	refuse         bool
	provider       string
	providerMarker string
	commandLog     string
	statusPath     string
	pueueConfig    string
}

func newTeamsScheduleFixture(t *testing.T) *teamsScheduleFixture {
	t.Helper()
	a := scheduleCreateArgs(t)
	a.Auto, a.ScheduleID, a.ScheduleTimezone, a.ScheduleTimezoneSet = false, scheduleTestID, "UTC", true
	a.TeamScope, a.TeamID, a.MemberID = "user", "review", "primary"
	state, err := taskdir.InitStore(a.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	f := &teamsScheduleFixture{state: state, arguments: a, ref: task.TeamReference{Scope: "user", TeamID: "review"}, refuse: true}
	f.deps = Dependencies{TeamCatalogPath: filepath.Join(t.TempDir(), "catalog", "teams.sqlite"), Environment: []string{}, ResourceChecker: fixtureResources}
	f.deps.Catalog = teamsScheduleProviderCatalog(t, commonprovider.ReadyCandidate(f.prepare))
	f.deps.PrepareCandidate = commonprovider.ReadyCandidate(f.prepare)
	f.catalog = teams.New(f.deps.TeamCatalogPath, nil)
	_, f.catalogID, err = f.catalog.Register(t.Context(), f.ref, teamsScheduleDefinition("alpha:run", "model-one", "high"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func teamsScheduleDefinition(provider, model, effort string) teamcontract.Team {
	return teamcontract.Team{
		SchemaVersion: 1, ID: "review", Name: "Review", Purpose: "Review scheduled work",
		Roles: []teamcontract.Role{{ID: "reviewer", Name: "Reviewer", Responsibility: "Find defects"}},
		Members: []teamcontract.Member{{
			ID: "primary", Name: "Primary", RoleIDs: []string{"reviewer"},
			ExecutionDefaults: &teamcontract.ExecutionDefaults{Provider: provider, Model: model, Effort: effort},
		}},
	}
}

func teamsScheduleProviderCatalog(t *testing.T, prepare commonprovider.PrepareCandidate) commonprovider.Catalog {
	t.Helper()
	base := preflightFixtureCatalog(t, prepare).Descriptions()[0]
	base.SupportedOptions = append(base.SupportedOptions, commonprovider.OptionModel, commonprovider.OptionEffort)
	registrations := make([]commonprovider.Registration, 0, 2)
	for _, id := range []string{"alpha:run", "beta:run"} {
		description := base
		description.ID = id
		registrations = append(registrations, commonprovider.Registration{
			Description: description, Prepare: prepare,
			Interpreters: []predicate.Interpreter{autoTestInterpreter{reference: preflightFixturePredicate(id)}},
		})
	}
	catalog, err := commonprovider.NewCatalog(registrations...)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func (f *teamsScheduleFixture) prepare(request task.TaskRecord) (PreparedProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request.Team = request.Team.Clone()
	f.requests = append(f.requests, request)
	if f.refuse {
		return PreparedProfile{}, errTeamsScheduleRefusal
	}
	profile := appTestExecutionProfile(&request, f.provider, []string{f.providerMarker})
	profile.Plan.Predicate = preflightFixturePredicate(request.Provider)
	return profile, nil
}

func (f *teamsScheduleFixture) captured() []task.TaskRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]task.TaskRecord(nil), f.requests...)
}

func (f *teamsScheduleFixture) create(t *testing.T) schedulecontract.Definition {
	t.Helper()
	result := createTeamScheduleCommand(f.arguments, f.deps)
	if result.err != nil || result.code != 0 {
		t.Fatalf("Team schedule create: code=%d err=%v", result.code, result.err)
	}
	f.reopen(t)
	row, err := f.store.GetSchedule(t.Context(), f.arguments.ScheduleID)
	if err != nil {
		t.Fatal(err)
	}
	return row.Definition
}

func (f *teamsScheduleFixture) reopen(t *testing.T) {
	t.Helper()
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	store, err := scheduler.Open(t.Context(), f.state)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}

func (f *teamsScheduleFixture) tasks() schedulerTasks {
	deps := f.deps.normalized()
	return schedulerTasks{
		root: f.state.Root, rootID: f.state.RootID,
		arguments: Arguments{Runner: f.deps.RunnerExecutable, PueueConfig: f.pueueConfig}, dependencies: deps, routing: deps.infrastructureRouting(),
	}
}

func (f *teamsScheduleFixture) persistTasks(t *testing.T) {
	t.Helper()
	client, options, statusPath, commandLog := newOrdinarySubmissionReceiptSupervisor(t)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	f.pueueConfig, f.statusPath, f.commandLog = client.Binding().ConfigPath, statusPath, commandLog
	deps := f.deps
	deps.InitialSupervisorExecutable, deps.SupervisorOptions, deps.Environment = client.Binding().ClientExecutable, options, options.Environment
	deps.SupervisorOptions.WaitForPendingClose, deps.PublisherVersion = true, "teams-schedule-test"
	paths := supervisorfixture.CanonicalTempDir(t)
	deps.RunnerExecutable = filepath.Join(paths, "runner")
	writeRunnerFixture(t, deps.RunnerExecutable, "#!/bin/sh\nexit 98\n")
	f.provider, f.providerMarker = filepath.Join(paths, "provider"), filepath.Join(paths, "provider-launched")
	writeRunnerFixture(t, f.provider, "#!/bin/sh\nprintf 'launched' >> \"$1\"\nexit 97\n")
	f.deps, f.refuse = deps, false
}

func (f *teamsScheduleFixture) dispatch(t *testing.T, id string) (schedulerruntime.TaskState, error) {
	t.Helper()
	definition := f.claim(t, id)
	state, dispatchErr := f.tasks().Dispatch(schedulerruntime.DispatchRequest{TaskID: id, Definition: definition})
	if state.Observation.ObservedAt.IsZero() {
		t.Fatalf("scheduler returned no observation: %+v %v", state, dispatchErr)
	}
	if err := f.store.RecordObservation(t.Context(), schedulecontract.OccurrenceObservation{TaskID: id, Observation: state.Observation}); err != nil {
		t.Fatal(err)
	}
	if state.Release != nil {
		if err := f.store.Release(t.Context(), schedulecontract.OccurrenceRelease{TaskID: id, Release: *state.Release}); err != nil {
			t.Fatal(err)
		}
	}
	return state, dispatchErr
}

func (f *teamsScheduleFixture) claim(t *testing.T, id string) schedulecontract.Definition {
	t.Helper()
	row, err := f.store.GetSchedule(t.Context(), f.arguments.ScheduleID)
	if err != nil {
		t.Fatal(err)
	}
	due := row.Cursor.Add(time.Minute)
	claim, err := f.store.ClaimAndArm(t.Context(), schedulecontract.Claim{
		ScheduleID:     row.Definition.ScheduleID,
		ExpectedCursor: row.Cursor, Due: due, TaskID: id, ArmedAt: due,
	})
	if err != nil || !claim.AuthorizesInvocation() {
		t.Fatalf("production occurrence claim: %+v %v", claim, err)
	}
	return row.Definition
}

func (f *teamsScheduleFixture) update(t *testing.T, definition teamcontract.Team) {
	t.Helper()
	current, _, err := f.catalog.Lookup(t.Context(), f.ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, updateErr := f.catalog.Update(t.Context(), f.ref, current.Revision, definition); updateErr != nil {
		t.Fatal(updateErr)
	}
}

func teamsScheduleStoredRequest(t *testing.T, root, id string) ([]byte, string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(root, ".scheduler", "schedules.sqlite")), RawQuery: "mode=ro"}
	connector, err := sqlite.NewConnector(uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	var data []byte
	var digest string
	if err := db.QueryRowContext(t.Context(), "SELECT request_json, definition_sha256 FROM schedules WHERE schedule_id = ?", id).Scan(&data, &digest); err != nil {
		t.Fatal(err)
	}
	return data, digest
}

func teamsScheduleAssertNoLaunch(t *testing.T, f *teamsScheduleFixture, ids ...string) {
	t.Helper()
	for _, id := range ids {
		for _, name := range []string{"provider.start", "provider.started.json", "provider.exit", "outcome.json"} {
			if _, err := os.Lstat(filepath.Join(f.state.Root, "tasks", id, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected launch/publication evidence %s: %v", name, err)
			}
		}
	}
	if f.providerMarker != "" {
		if _, err := os.Stat(f.providerMarker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fixture provider executed: %v", err)
		}
	}
}

// Used only to prove saved non-Git project roots; no native provider runs.
func teamsScheduleCanonicalDirectory(t *testing.T, path string) string {
	t.Helper()
	canonical, err := config.CanonicalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
