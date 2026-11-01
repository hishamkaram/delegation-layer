package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func teamsCommandDependencies(t *testing.T) Dependencies {
	t.Helper()
	return Dependencies{TeamCatalogPath: filepath.Join(t.TempDir(), "catalog", "teams.sqlite"), Environment: []string{}}
}

func runTeamsForTest(t *testing.T, deps Dependencies, definition string, argv ...string) (int, TeamsResponse) {
	t.Helper()
	deps.Input = strings.NewReader(definition)
	var stdout, stderr bytes.Buffer
	args := append([]string{"teams"}, argv...)
	args = append(args, "--json")
	exit := Run(args, &stdout, &stderr, deps)
	var response TeamsResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("invalid response: exit=%d stdout=%s stderr=%s: %v", exit, stdout.String(), stderr.String(), err)
	}
	return exit, response
}

func requireTeamsRevision(t *testing.T, deps Dependencies, revision int64, name string) {
	t.Helper()
	exit, response := runTeamsForTest(t, deps, "", "show", "--scope", "user", "--team", "review")
	if exit != 0 || response.Team == nil || response.Team.Revision != revision || response.Team.Definition.Name != name {
		t.Fatalf("wrong durable state: exit=%d response=%+v", exit, response)
	}
}

func requireTeamsMutation(t *testing.T, exit int, response TeamsResponse, operation string, revision int64) {
	t.Helper()
	if exit != 0 || response.Mutation == nil || response.Mutation.Operation != operation || response.Mutation.Revision != revision {
		t.Fatalf("%s: exit=%d response=%+v", operation, exit, response)
	}
}

func requireTeamsRegistration(t *testing.T, response TeamsResponse) {
	t.Helper()
	if response.Mutation.Reference == nil || response.Mutation.Reference.TeamID != "review" || response.CatalogID == "" {
		t.Fatalf("registration lost identity: %+v", response)
	}
	if response.Mutation.Validation == nil || response.Mutation.Validation.Readiness != "not_checked" {
		t.Fatalf("mutation claims readiness: %+v", response.Mutation)
	}
}

func TestTeamsCLIStdinCRUDUsesProductionCatalog(t *testing.T) {
	deps := teamsCommandDependencies(t)
	exit, response := runTeamsForTest(t, deps, teamsDefinitionJSON, "register", "--scope", "user", "--definition", "-")
	requireTeamsMutation(t, exit, response, "register", 1)
	requireTeamsRegistration(t, response)
	requireTeamsRevision(t, deps, 1, "Review")
	exit, response = runTeamsForTest(t, deps, "", "list")
	if exit != 0 || response.Teams == nil || len(*response.Teams) != 1 || (*response.Teams)[0].MemberCount != 1 || response.Default != nil {
		t.Fatalf("list: exit=%d response=%+v", exit, response)
	}
	updated := strings.Replace(teamsDefinitionJSON, `"name":"Review"`, `"name":"Updated"`, 1)
	exit, response = runTeamsForTest(t, deps, updated, "update", "--scope", "user", "--team", "review", "--expected-revision", "1", "--definition", "-")
	requireTeamsMutation(t, exit, response, "update", 2)
	requireTeamsRevision(t, deps, 2, "Updated")
	exit, response = runTeamsForTest(t, deps, "", "delete", "--scope", "user", "--team", "review", "--expected-revision", "2")
	requireTeamsMutation(t, exit, response, "delete", 3)
	exit, response = runTeamsForTest(t, deps, "", "show", "--scope", "user", "--team", "review")
	if exit != 2 || response.Failure == nil || response.Failure.Code != "not_found" {
		t.Fatalf("deleted target: exit=%d response=%+v", exit, response)
	}
	exit, response = runTeamsForTest(t, deps, teamsDefinitionJSON, "register", "--scope", "user", "--definition", "-")
	requireTeamsMutation(t, exit, response, "register", 4)
}

func TestTeamsCLIRejectedMutationsPreserveAuthoritativeState(t *testing.T) {
	deps := teamsCommandDependencies(t)
	exit, response := runTeamsForTest(t, deps, teamsDefinitionJSON, "register", "--scope", "user", "--definition", "-")
	if exit != 0 {
		t.Fatalf("register: %+v", response)
	}
	mismatched := strings.Replace(teamsDefinitionJSON, `"id":"review"`, `"id":"other"`, 1)
	tests := []struct {
		definition string
		argv       []string
		code       string
	}{
		{teamsDefinitionJSON, []string{"register", "--scope", "user", "--definition", "-"}, "revision_conflict"},
		{mismatched, []string{"update", "--scope", "user", "--team", "review", "--expected-revision", "1", "--definition", "-"}, "invalid_definition"},
		{teamsDefinitionJSON, []string{"update", "--scope", "user", "--team", "review", "--expected-revision", "2", "--definition", "-"}, "revision_conflict"},
		{"", []string{"delete", "--scope", "user", "--team", "review", "--expected-revision", "2"}, "revision_conflict"},
	}
	for _, test := range tests {
		exit, response = runTeamsForTest(t, deps, test.definition, test.argv...)
		if exit != 2 || response.Failure == nil || response.Failure.Code != test.code || response.Mutation != nil {
			t.Fatalf("rejected mutation: exit=%d response=%+v", exit, response)
		}
		requireTeamsRevision(t, deps, 1, "Review")
	}
}

func TestTeamsCLIPassiveReadsDoNotInitializeCatalogOrExecutionRoot(t *testing.T) {
	deps := teamsCommandDependencies(t)
	root := filepath.Join(t.TempDir(), "execution")
	exit, response := runTeamsForTest(t, deps, "", "--root", root, "list")
	if exit != 0 || response.Status != "empty" || response.Teams == nil || len(*response.Teams) != 0 {
		t.Fatalf("absent catalog: exit=%d response=%+v", exit, response)
	}
	exit, response = runTeamsForTest(t, deps, "", "--root", root, "show", "--scope", "user", "--team", "review")
	if exit != 2 || response.Failure == nil || response.Failure.Code != "not_found" {
		t.Fatalf("absent explicit target: exit=%d response=%+v", exit, response)
	}
	for _, path := range []string{root, filepath.Dir(deps.TeamCatalogPath)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("passive read created state at %s: %v", path, err)
		}
	}
}

func TestTeamsCLIInvalidDefinitionDoesNotCreateCatalog(t *testing.T) {
	deps := teamsCommandDependencies(t)
	for _, definition := range []string{`{}`, teamsDefinitionJSON + "{}", strings.Replace(teamsDefinitionJSON, `"role_ids":["reviewer"]`, `"role_ids":["reviewer"],"execution_defaults":{"provider":"unknown:print"}`, 1)} {
		exit, response := runTeamsForTest(t, deps, definition, "register", "--scope", "user", "--definition", "-")
		if exit != 2 || response.Failure == nil || response.Failure.Code != "invalid_definition" {
			t.Fatalf("invalid definition: exit=%d response=%+v", exit, response)
		}
		if _, err := os.Lstat(filepath.Dir(deps.TeamCatalogPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid registration created catalog: %v", err)
		}
	}
}

func TestTeamsCLIMutationOutputFailurePreservesCommittedResult(t *testing.T) {
	deps := teamsCommandDependencies(t)
	deps.Input = strings.NewReader(teamsDefinitionJSON)
	var stderr bytes.Buffer
	exit := Run([]string{"teams", "register", "--scope", "user", "--definition", "-", "--json"}, teamsBrokenWriter{}, &stderr, deps)
	if exit != 1 || !strings.Contains(stderr.String(), "mutation succeeded") || !strings.Contains(stderr.String(), "revision=1") || !strings.Contains(stderr.String(), "teams show") || !strings.Contains(stderr.String(), "do not repeat") {
		t.Fatalf("committed mutation hidden: exit=%d stderr=%s", exit, stderr.String())
	}
	requireTeamsRevision(t, deps, 1, "Review")
}

func TestTeamsCLIListPaging(t *testing.T) {
	deps := teamsCommandDependencies(t)
	for _, id := range []string{"zeta", "alpha", "middle"} {
		definition := strings.Replace(teamsDefinitionJSON, `"id":"review"`, `"id":"`+id+`"`, 1)
		if exit, response := runTeamsForTest(t, deps, definition, "register", "--scope", "user", "--definition", "-"); exit != 0 {
			t.Fatalf("register: %+v", response)
		}
	}
	exit, response := runTeamsForTest(t, deps, "", "list", "--limit", "2")
	requireTeamsPage(t, exit, response, []string{"alpha", "middle"}, "user:middle")
	exit, response = runTeamsForTest(t, deps, "", "list", "--limit", "2", "--after", response.NextCursor)
	requireTeamsPage(t, exit, response, []string{"zeta"}, "")
}

func requireTeamsPage(t *testing.T, exit int, response TeamsResponse, ids []string, cursor string) {
	t.Helper()
	if exit != 0 || response.Teams == nil || len(*response.Teams) != len(ids) || response.NextCursor != cursor {
		t.Fatalf("wrong list page: exit=%d response=%+v", exit, response)
	}
	for index, id := range ids {
		if (*response.Teams)[index].Reference.TeamID != id {
			t.Fatalf("page row %d: got %s want %s", index, (*response.Teams)[index].Reference.TeamID, id)
		}
	}
}

func TestTeamsCLIReadFaultIsNotEmpty(t *testing.T) {
	broken := teamsCommandDependencies(t)
	if err := os.MkdirAll(filepath.Dir(broken.TeamCatalogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken.TeamCatalogPath, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, response := runTeamsForTest(t, broken, "", "list")
	if exit != 1 || response.Failure == nil || response.Status != "failed" || response.Teams != nil {
		t.Fatalf("corrupt catalog treated as empty: exit=%d response=%+v", exit, response)
	}
}

func TestTeamsErrorExitCodeKeepsOperationalFaults(t *testing.T) {
	for _, err := range []error{contract.ErrInvalidDefinition, task.ErrInvalidTeamSelection, teams.ErrNotFound, teams.ErrConflict, teams.ErrCatalogIdentity, teams.ErrProjectContext, teams.ErrProjectMismatch} {
		if exit := TeamsErrorExitCode(err); exit != 2 {
			t.Fatalf("input error exit=%d: %v", exit, err)
		}
	}
	for _, err := range []error{
		teams.ErrUnavailable, teams.ErrSchema, context.Canceled,
		errors.Join(teams.ErrConflict, teams.ErrUnavailable),
		errors.Join(teams.ErrNotFound, &os.PathError{Op: "close", Path: "catalog", Err: os.ErrPermission}),
		errors.Join(contract.ErrInvalidDefinition, io.ErrClosedPipe),
		fmt.Errorf("lookup: %w", errors.Join(teams.ErrNotFound, io.ErrUnexpectedEOF)),
		errors.Join(teams.ErrConflict, errors.New("unclassified close fault")),
	} {
		if exit := TeamsErrorExitCode(err); exit != 1 {
			t.Fatalf("operational error exit=%d: %v", exit, err)
		}
	}
	if exit := TeamsErrorExitCode(nil); exit != 0 {
		t.Fatalf("success exit=%d", exit)
	}
}
