package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

const teamsDefinitionJSON = `{"schema_version":1,"id":"review","name":"Review","purpose":"Find regressions","roles":[{"id":"reviewer","name":"Reviewer","responsibility":"Check correctness"}],"members":[{"id":"primary","name":"Primary","role_ids":["reviewer"]}]}`

func TestTeamsDefinitionInputUsesStrictBoundedTransport(t *testing.T) {
	definition, err := readTeamsDefinition("-", strings.NewReader(teamsDefinitionJSON))
	if err != nil || definition.ID != "review" {
		t.Fatalf("stdin definition=%+v err=%v", definition, err)
	}
	path := filepath.Join(t.TempDir(), "definition.json")
	if err = os.WriteFile(path, []byte(teamsDefinitionJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	definition, err = readTeamsDefinition(path, strings.NewReader("ignored"))
	if err != nil || definition.ID != "review" {
		t.Fatalf("file definition=%+v err=%v", definition, err)
	}
	for _, data := range []string{
		"", teamsDefinitionJSON + `{}`, `{"schema_version":2}`, strings.Replace(teamsDefinitionJSON, `"purpose":`, `"unknown":`, 1),
	} {
		if _, err = readTeamsDefinition("-", strings.NewReader(data)); !errors.Is(err, contract.ErrInvalidDefinition) {
			t.Fatalf("invalid definition accepted or lost identity: %v", err)
		}
	}
	if _, err = readTeamsDefinition("-", strings.NewReader(strings.Repeat(" ", task.MaxControlRecordSize+1))); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized input: %v", err)
	}
	if _, err = readTeamsDefinition("-", nil); !errors.Is(err, contract.ErrInvalidDefinition) {
		t.Fatalf("missing injected input: %v", err)
	}
	if _, err = readTeamsDefinition(path+"-missing", nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file IO failure: %v", err)
	}
}

func TestTeamsPresentationPreservesTypedEmptyAndFullDefinition(t *testing.T) {
	rows := []TeamsListRow{}
	response := TeamsResponse{SchemaVersion: 1, Command: "teams", Action: "list", Status: "empty", Teams: &rows}
	var output bytes.Buffer
	if err := writeTeamsResponse(&output, response, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"teams":[]`) || strings.Contains(output.String(), `"default"`) {
		t.Fatalf("empty list/default confused: %s", output.String())
	}
	definition, err := contract.Decode([]byte(teamsDefinitionJSON))
	if err != nil {
		t.Fatal(err)
	}
	response = TeamsResponse{SchemaVersion: 1, Command: "teams", Action: "show", Status: "ok", Team: &contract.Record{Reference: task.TeamReference{Scope: "user", TeamID: "review"}, Revision: 3, Definition: definition}}
	output.Reset()
	if err = writeTeamsResponse(&output, response, true); err != nil {
		t.Fatal(err)
	}
	var decoded TeamsResponse
	if err = json.Unmarshal(output.Bytes(), &decoded); err != nil || decoded.Team == nil || decoded.Team.Definition.Members[0].ID != "primary" || decoded.Team.Revision != 3 {
		t.Fatalf("typed show lost content: %+v, %v", decoded, err)
	}
	output.Reset()
	if err = writeTeamsResponse(&output, response, false); err != nil || !strings.Contains(output.String(), "responsibility=\"Check correctness\"") || !strings.Contains(output.String(), "member id=primary") {
		t.Fatalf("human show incomplete: %s, %v", output.String(), err)
	}
}

type teamsBrokenWriter struct{}

func (teamsBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestTeamsPresentationPropagatesWriterFailure(t *testing.T) {
	response := TeamsResponse{SchemaVersion: 1, Command: "teams", Action: "list", Status: "empty"}
	if err := writeTeamsResponse(teamsBrokenWriter{}, response, true); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost output failure: %v", err)
	}
}
