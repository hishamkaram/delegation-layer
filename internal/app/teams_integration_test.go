package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/teams"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func integrationTeam(t *testing.T, path string) (*teams.Catalog, teamcontract.Record, task.TeamTarget) {
	t.Helper()
	catalog := teams.New(path, nil)
	definition, err := teamcontract.Decode([]byte(teamsDefinitionJSON))
	if err != nil {
		t.Fatal(err)
	}
	definition.Members[0].ExecutionDefaults = &teamcontract.ExecutionDefaults{Provider: "fixture:test"}
	record, id, err := catalog.Register(t.Context(), task.TeamReference{Scope: "user", TeamID: definition.ID}, definition)
	if err != nil {
		t.Fatal(err)
	}
	target := task.TeamTarget{Team: record.Reference.Clone(), MemberID: definition.Members[0].ID, CatalogID: id}
	return catalog, record, target
}

func persistedIntegrationTeam(t *testing.T, store *taskdir.Store, template *taskdir.TaskDir, original *task.TaskRecord, target task.TeamTarget, catalog *teams.Catalog) (*taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	member, err := catalog.ResolveMember(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID = strings.Repeat("d", 32)
	request.Team, err = teamcontract.SelectionContext(member, task.TeamIntent{Config: task.TaskConfig{Permission: config.ModeReadOnly}})
	if err != nil {
		t.Fatal(err)
	}
	_, meta, err := template.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	meta.TaskID = request.TaskID
	td, err := store.CreateTask(request.TaskID, &request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles("answer\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("publication: %v/%v", cleanupErr, collectErr)
	}
	return td, &request
}

func TestTeamsPersistedInspectionAndReplayIgnoreDeletedCatalog(t *testing.T) {
	store, template, original := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, template) })
	path := filepath.Join(t.TempDir(), "catalog", "teams.sqlite")
	catalog, record, target := integrationTeam(t, path)
	td, request := persistedIntegrationTeam(t, store, template, original, target, catalog)
	before, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Delete(t.Context(), record.Reference, record.Revision); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{TeamCatalogPath: path, ResourceChecker: fixtureResources, SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { t.Fatal("terminal observation launched supervisor") }}}
	assertTeamTerminalInspection(t, store, request, deps)
	replay := dispatch(Arguments{Root: store.Root, TaskID: request.TaskID, Cwd: request.CanonicalCwd, BriefBytes: []byte("brief"), TeamScope: "user", TeamID: target.Team.TeamID, MemberID: target.MemberID, Config: task.TaskConfig{Permission: config.ModeReadOnly}}, deps)
	if replay.code != 0 || replay.err != nil || replay.response.Publication != "committed" || !reflect.DeepEqual(replay.response.Team, request.Team) {
		t.Fatalf("replay read deleted catalog or changed settings: %+v", replay)
	}
	after, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("inspection/replay rewrote immutable request: %v", err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive operation recreated catalog: %v", err)
	}
}

func TestTeamsNewDirectAttemptsResolveLatestWithoutLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog", "teams.sqlite")
	catalog, record, target := integrationTeam(t, path)
	var observed []task.TaskRecord
	refusal := commonprovider.ErrAuthenticationBlocked
	providerCatalog := preflightFixtureCatalog(t, func(request task.TaskRecord, _ []string) (commonprovider.ProfileCandidate, error) {
		observed = append(observed, request)
		return commonprovider.ProfileCandidate{}, refusal
	})
	// The fixture advertises model/effort support but always refuses before a
	// supervisor or provider starts. It observes the production admission input.
	registration, err := providerCatalog.Lookup("fixture:preflight")
	if err != nil {
		t.Fatal(err)
	}
	registration.Description.SupportedOptions = append(registration.Description.SupportedOptions, commonprovider.OptionModel, commonprovider.OptionEffort)
	providerCatalog, err = commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	root, cwd := filepath.Join(t.TempDir(), "state"), t.TempDir()
	deps := Dependencies{TeamCatalogPath: path, Catalog: providerCatalog, ResourceChecker: fixtureResources, Environment: os.Environ()}
	for i, model := range []string{"first-model", "latest-model"} {
		definition := record.Definition.Clone()
		definition.Members[0].ExecutionDefaults = &teamcontract.ExecutionDefaults{Provider: "fixture:preflight", Model: model, Effort: "high"}
		record, _, err = catalog.Update(t.Context(), record.Reference, record.Revision, definition)
		if err != nil {
			t.Fatal(err)
		}
		result := dispatch(Arguments{Root: root, TaskID: strings.Repeat(string(rune('a'+i)), 32), Cwd: cwd, BriefBytes: []byte("brief"), TeamScope: "user", TeamID: target.Team.TeamID, MemberID: target.MemberID, Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"}}, deps)
		if !errors.Is(result.err, refusal) || len(observed) != i+1 {
			t.Fatalf("did not reach bounded fixture: %+v, calls=%d", result, len(observed))
		}
		req := observed[i]
		if req.RequestedConfig.Model != model || req.Team == nil || req.Team.TeamRevision != record.Revision || req.Team.Intent.Config.Model != "" {
			t.Fatalf("new attempt did not resolve latest once: %+v", req)
		}
		if _, err = os.Stat(filepath.Join(root, "tasks", req.TaskID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refusal created task/provider authority: %v", err)
		}
	}
}

func TestTeamsRemainOptionalWhenCatalogCannotBeResolved(t *testing.T) {
	calls := 0
	refusal := commonprovider.ErrAuthenticationBlocked
	providerCatalog := preflightFixtureCatalog(t, func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
		calls++
		return commonprovider.ProfileCandidate{}, refusal
	})
	result := dispatch(Arguments{Root: filepath.Join(t.TempDir(), "state"), TaskID: strings.Repeat("f", 32), Provider: "fixture:preflight", Cwd: t.TempDir(), BriefBytes: []byte("brief"), Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"}}, Dependencies{TeamCatalogPath: "invalid\x00catalog", Environment: []string{}, ResourceChecker: fixtureResources, Catalog: providerCatalog})
	if !errors.Is(result.err, refusal) || calls != 1 {
		t.Fatalf("ordinary dispatch depended on Team catalog: %+v, calls=%d", result, calls)
	}
}

func assertTeamTerminalInspection(t *testing.T, store *taskdir.Store, request *task.TaskRecord, deps Dependencies) {
	t.Helper()
	for _, command := range []string{"status", "collect"} {
		var output, diagnostic bytes.Buffer
		code := Run([]string{command, request.TaskID, "--root", store.Root, "--json"}, &output, &diagnostic, deps)
		var response Response
		if decodeErr := json.Unmarshal(output.Bytes(), &response); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if code != 0 || response.Publication != "committed" || !reflect.DeepEqual(response.Team, request.Team) {
			t.Fatalf("%s code=%d response=%+v stderr=%s", command, code, response, diagnostic.String())
		}
	}
}
