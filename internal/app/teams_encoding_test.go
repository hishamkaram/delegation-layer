package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams"
)

func teamsEncodingDefinition(modelJSON []byte) string {
	member := `"role_ids":["reviewer"],"execution_defaults":{"provider":"codex:exec","model":` + string(modelJSON) + `}`
	return strings.Replace(teamsDefinitionJSON, `"role_ids":["reviewer"]`, member, 1)
}

func TestTeamsCLIInvalidUnicodeMutationsPreserveCatalog(t *testing.T) {
	for _, test := range []struct {
		name  string
		model []byte
	}{
		{"literal UTF-8", []byte{'"', 'm', 0xff, '"'}},
		{"unpaired high surrogate", []byte(`"vendor/\ud800"`)},
		{"unpaired low surrogate", []byte(`"vendor/\udfff"`)},
	} {
		for _, action := range []string{"register", "update"} {
			t.Run(test.name+"/"+action, func(t *testing.T) {
				teamsEncodingCheckRejectedMutation(t, action, test.model)
			})
		}
	}
}

func teamsEncodingCheckRejectedMutation(t *testing.T, action string, model []byte) {
	t.Helper()
	deps := teamsCommandDependencies(t)
	exit, response := runTeamsForTest(t, deps, teamsEncodingDefinition([]byte(`"original-model"`)), "register", "--scope", "user", "--definition", "-")
	requireTeamsMutation(t, exit, response, "register", 1)
	catalog := teams.New(deps.TeamCatalogPath, nil)
	ref := task.TeamReference{Scope: "user", TeamID: "review"}
	prior, catalogID, err := catalog.Lookup(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(deps.TeamCatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	definition := teamsEncodingDefinition(model)
	argv := []string{action, "--scope", "user", "--definition", "-"}
	if action == "register" {
		definition = strings.Replace(definition, `"id":"review"`, `"id":"new-team"`, 1)
	} else {
		argv = append(argv, "--team", "review", "--expected-revision", "1")
	}
	exit, response = runTeamsForTest(t, deps, definition, argv...)
	if exit != 2 || response.Failure == nil || response.Failure.Code != "invalid_definition" || response.Mutation != nil {
		t.Fatalf("CLI accepted Unicode repair: exit=%d response=%+v", exit, response)
	}
	current, currentID, err := catalog.Lookup(t.Context(), ref)
	if err != nil || currentID != catalogID || !reflect.DeepEqual(current, prior) {
		t.Fatalf("rejected encoding changed the prior Team or revision: %+v %v", current, err)
	}
	after, err := os.ReadFile(deps.TeamCatalogPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected encoding changed SQLite bytes: %v", err)
	}
	listing, err := catalog.List(t.Context(), nil)
	if err != nil || len(listing.Records) != 1 {
		t.Fatalf("rejected registration added a repaired Team: %+v %v", listing, err)
	}
}

func TestTeamsCLIInvalidUnicodeDoesNotInitializeCatalog(t *testing.T) {
	deps := teamsCommandDependencies(t)
	definition := teamsEncodingDefinition([]byte(`"vendor/\ud800"`))
	exit, response := runTeamsForTest(t, deps, definition, "register", "--scope", "user", "--definition", "-")
	if exit != 2 || response.Failure == nil || response.Failure.Code != "invalid_definition" {
		t.Fatalf("invalid first registration: exit=%d response=%+v", exit, response)
	}
	if _, err := os.Lstat(filepath.Dir(deps.TeamCatalogPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid encoding initialized the catalog: %v", err)
	}
}

func TestTeamsCLIValidUnicodeSelectorsRemainLiteral(t *testing.T) {
	for _, test := range []struct{ name, modelJSON, want string }{
		{"explicit replacement rune", `"vendor/�"`, "vendor/\ufffd"},
		{"paired surrogates", `"vendor/\ud83d\ude00"`, "vendor/\U0001f600"},
		{"escaped literal backslash-u", `"vendor/\\ud800"`, `vendor/\ud800`},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := teamsCommandDependencies(t)
			definition := teamsEncodingDefinition([]byte(test.modelJSON))
			exit, response := runTeamsForTest(t, deps, definition, "register", "--scope", "user", "--definition", "-")
			requireTeamsMutation(t, exit, response, "register", 1)
			exit, response = runTeamsForTest(t, deps, definition, "update", "--scope", "user", "--team", "review", "--expected-revision", "1", "--definition", "-")
			requireTeamsMutation(t, exit, response, "update", 2)
			catalog := teams.New(deps.TeamCatalogPath, nil)
			record, _, err := catalog.Lookup(t.Context(), task.TeamReference{Scope: "user", TeamID: "review"})
			if err != nil {
				t.Fatal(err)
			}
			if got := record.Definition.Members[0].ExecutionDefaults.Model; got != test.want {
				t.Fatalf("valid literal selector changed in SQLite: got %q want %q", got, test.want)
			}
		})
	}
}
