package contract

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	testRoleJSON     = `{"id":"reviewer","name":"Reviewer","responsibility":"Check correctness."}`
	testDefaultsJSON = `{"provider":"future-agent:exec","model":"vendor/model-2027:exact","effort":"experimental"}`
	testMemberJSON   = `{"id":"primary","name":"Primary","role_ids":["reviewer"]}`
)

func teamDefinitionFixture() Team {
	return Team{
		SchemaVersion: 1, ID: "review-team", Name: "Review team", Purpose: "Review changes — carefully.",
		Roles: []Role{
			{ID: "reviewer", Name: "Reviewer", Responsibility: "Check correctness."},
			{ID: "author", Name: "Author", Responsibility: "Explain the change."},
		},
		Members: []Member{
			{
				ID: "primary", Name: "Primary", RoleIDs: []string{"author", "reviewer"},
				ExecutionDefaults: &ExecutionDefaults{Provider: "future-agent:exec", Model: "vendor/model-2027:exact", Effort: "experimental"},
			},
			{ID: "unbound", Name: "Unbound", RoleIDs: []string{"reviewer"}},
		},
	}
}

func teamDefinitionDocument(roles, members string) string {
	return `{"schema_version":1,"id":"review-team","name":"Review team","purpose":"Review changes.","roles":` + roles + `,"members":` + members + `}`
}

func definitionFields(t *testing.T, data string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func definitionFieldJSON(t *testing.T, fields map[string]json.RawMessage) string {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func requireInvalidDefinitionJSON(t *testing.T, document string) {
	t.Helper()
	got, err := Decode([]byte(document))
	if !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("Decode error = %v, want ErrInvalidDefinition", err)
	}
	if !reflect.DeepEqual(got, Team{}) {
		t.Fatalf("Decode returned a partial definition on failure: %+v", got)
	}
}

func TestDecodeTeamStrictFields(t *testing.T) {
	wrapMember := func(member string) string { return teamDefinitionDocument("["+testRoleJSON+"]", "["+member+"]") }
	cases := []struct {
		name, object string
		wrap         func(string) string
		required     []string
	}{
		{"team", teamDefinitionDocument("[]", "[]"), func(s string) string { return s }, []string{"schema_version", "id", "name", "purpose", "roles", "members"}},
		{"role", testRoleJSON, func(s string) string { return teamDefinitionDocument("["+s+"]", "[]") }, []string{"id", "name", "responsibility"}},
		{"member", testMemberJSON, wrapMember, []string{"id", "name", "role_ids"}},
		{"defaults", testDefaultsJSON, func(s string) string {
			return wrapMember(`{"id":"primary","name":"Primary","role_ids":["reviewer"],"execution_defaults":` + s + `}`)
		}, []string{"provider"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.wrap(tc.object))); err != nil {
				t.Fatalf("valid control rejected: %v", err)
			}
			fields := definitionFields(t, tc.object)
			fields["unexpected"] = json.RawMessage(`true`)
			t.Run("unknown", func(t *testing.T) {
				requireInvalidDefinitionJSON(t, tc.wrap(definitionFieldJSON(t, fields)))
			})
			for _, field := range tc.required {
				t.Run("missing/"+field, func(t *testing.T) {
					fields := definitionFields(t, tc.object)
					delete(fields, field)
					requireInvalidDefinitionJSON(t, tc.wrap(definitionFieldJSON(t, fields)))
				})
				t.Run("null/"+field, func(t *testing.T) {
					fields := definitionFields(t, tc.object)
					fields[field] = json.RawMessage(`null`)
					requireInvalidDefinitionJSON(t, tc.wrap(definitionFieldJSON(t, fields)))
				})
			}
		})
	}
}

func TestDecodeTeamRejectsExecutionAuthorityInDefaults(t *testing.T) {
	for _, field := range []string{"permission", "budget", "workspace", "argv", "shell", "credentials"} {
		t.Run(field, func(t *testing.T) {
			fields := definitionFields(t, testDefaultsJSON)
			fields[field] = json.RawMessage(`"value"`)
			member := `{"id":"primary","name":"Primary","role_ids":["reviewer"],"execution_defaults":` + definitionFieldJSON(t, fields) + `}`
			requireInvalidDefinitionJSON(t, teamDefinitionDocument("["+testRoleJSON+"]", "["+member+"]"))
		})
	}
}

func TestDecodeTeamRejectsAmbiguousDocuments(t *testing.T) {
	valid := teamDefinitionDocument("[]", "[]")
	cases := []string{
		`null`, `[]`, ``, valid + `{}`, valid + ` trailing`,
		`{"schema_version":1,"schema_version":1,"id":"review-team","name":"Review team","purpose":"Review changes.","roles":[],"members":[]}`,
		`{"schema_version":1,"id":"review-team","ID":"review-team","name":"Review team","purpose":"Review changes.","roles":[],"members":[]}`,
		`{"Schema_version":1,"id":"review-team","name":"Review team","purpose":"Review changes.","roles":[],"members":[]}`,
		teamDefinitionDocument(`[{"id":"reviewer","name":"Reviewer","name":"Reviewer","responsibility":"Check correctness."}]`, `[]`),
	}
	for _, document := range cases {
		t.Run(document, func(t *testing.T) { requireInvalidDefinitionJSON(t, document) })
	}
}

func TestTeamRequiredValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Team)
	}{
		{"missing version", func(v *Team) { v.SchemaVersion = 0 }},
		{"future version", func(v *Team) { v.SchemaVersion = 2 }},
		{"missing id", func(v *Team) { v.ID = "" }},
		{"invalid id", func(v *Team) { v.ID = "Review-Team" }},
		{"blank name", func(v *Team) { v.Name = " \t" }},
		{"blank purpose", func(v *Team) { v.Purpose = "\n" }},
		{"invalid name UTF-8", func(v *Team) { v.Name = "\xff" }},
		{"invalid purpose UTF-8", func(v *Team) { v.Purpose = "\xff" }},
		{"missing roles", func(v *Team) { v.Roles = nil }},
		{"missing members", func(v *Team) { v.Members = nil }},
		{"blank role name", func(v *Team) { v.Roles[0].Name = " " }},
		{"blank responsibility", func(v *Team) { v.Roles[0].Responsibility = " " }},
		{"invalid role UTF-8", func(v *Team) { v.Roles[0].Name = "\xff" }},
		{"invalid responsibility UTF-8", func(v *Team) { v.Roles[0].Responsibility = "\xff" }},
		{"blank member name", func(v *Team) { v.Members[0].Name = " " }},
		{"invalid member UTF-8", func(v *Team) { v.Members[0].Name = "\xff" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			team := teamDefinitionFixture()
			tc.mutate(&team)
			if err := Validate(team); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("Validate error = %v, want ErrInvalidDefinition", err)
			}
			if data, err := Encode(team); !errors.Is(err, ErrInvalidDefinition) || data != nil {
				t.Fatalf("Encode = %q, %v; want no data and ErrInvalidDefinition", data, err)
			}
		})
	}
}

func TestTeamRoleIntegrity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Team)
	}{
		{"duplicate role", func(v *Team) { v.Roles = append(v.Roles, v.Roles[0]) }},
		{"duplicate member", func(v *Team) { v.Members = append(v.Members, v.Members[0]) }},
		{"invalid role id", func(v *Team) { v.Roles[0].ID = "Reviewer" }},
		{"invalid member id", func(v *Team) { v.Members[0].ID = "Primary" }},
		{"dangling role", func(v *Team) { v.Members[0].RoleIDs = []string{"reviewer", "missing"} }},
		{"repeated role", func(v *Team) { v.Members[0].RoleIDs = []string{"reviewer", "reviewer"} }},
		{"missing role references", func(v *Team) { v.Members[0].RoleIDs = nil }},
		{"empty role references", func(v *Team) { v.Members[0].RoleIDs = []string{} }},
		{"no declared roles", func(v *Team) { v.Roles = []Role{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			team := teamDefinitionFixture()
			tc.mutate(&team)
			if err := Validate(team); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("Validate error = %v, want ErrInvalidDefinition", err)
			}
		})
	}
	t.Run("entity namespaces are separate", func(t *testing.T) {
		team := teamDefinitionFixture()
		team.ID, team.Members[0].ID = "reviewer", "reviewer"
		if err := Validate(team); err != nil {
			t.Fatalf("IDs reused across namespaces rejected: %v", err)
		}
	})
}

func TestTeamExecutionDefaultsLiteralValues(t *testing.T) {
	cases := []struct {
		name     string
		defaults *ExecutionDefaults
		valid    bool
	}{
		{"unbound", nil, true},
		{"provider only", &ExecutionDefaults{Provider: "future-agent:exec"}, true},
		{"unlisted literal model and effort", &ExecutionDefaults{Provider: "future-agent:exec", Model: "vendor/model-2027:exact", Effort: "experimental"}, true},
		{"exact provider alias", &ExecutionDefaults{Provider: "claude:print", Model: "opus"}, true},
		{"missing provider", &ExecutionDefaults{}, false},
		{"model without provider", &ExecutionDefaults{Model: "exact-model"}, false},
		{"malformed provider", &ExecutionDefaults{Provider: "codex"}, false},
		{"padded provider", &ExecutionDefaults{Provider: " codex:exec"}, false},
		{"padded model", &ExecutionDefaults{Provider: "codex:exec", Model: " exact-model"}, false},
		{"padded effort", &ExecutionDefaults{Provider: "codex:exec", Effort: "high\n"}, false},
		{"NUL model", &ExecutionDefaults{Provider: "codex:exec", Model: "model\x00"}, false},
		{"NUL effort", &ExecutionDefaults{Provider: "codex:exec", Effort: "hi\x00gh"}, false},
		{"invalid model UTF-8", &ExecutionDefaults{Provider: "codex:exec", Model: "\xff"}, false},
		{"invalid effort UTF-8", &ExecutionDefaults{Provider: "codex:exec", Effort: "\xff"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			team := teamDefinitionFixture()
			team.Members[0].ExecutionDefaults = tc.defaults
			err := Validate(team)
			if tc.valid {
				if err != nil {
					t.Fatalf("valid defaults rejected: %v", err)
				}
			} else if !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("Validate error = %v, want ErrInvalidDefinition", err)
			}
		})
	}
}

func TestTeamCodecPreservesSemanticValues(t *testing.T) {
	want := teamDefinitionFixture()
	data, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	var wire Team
	if err = json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wire, want) {
		t.Fatalf("encoded values = %+v, want %+v", wire, want)
	}
	got, err := Decode(data)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded values = %+v, %v; want %+v", got, err, want)
	}
	fields := definitionFields(t, string(data))
	var members []map[string]json.RawMessage
	if err := json.Unmarshal(fields["members"], &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("encoded %d members, want 2", len(members))
	}
	if _, present := members[1]["execution_defaults"]; present {
		t.Fatal("unbound member's defaults must be omitted")
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("canonical definition must end in a newline")
	}
}

func TestTeamExplicitEmptyArrays(t *testing.T) {
	for _, roles := range []string{"[]", "[" + testRoleJSON + "]"} {
		t.Run(roles, func(t *testing.T) {
			team, err := Decode([]byte(teamDefinitionDocument(roles, "[]")))
			if err != nil {
				t.Fatalf("empty team rejected: %v", err)
			}
			for _, value := range []Team{team, team.Clone()} {
				requireExplicitTeamArrays(t, value)
			}
		})
	}
}

func requireExplicitTeamArrays(t *testing.T, value Team) {
	t.Helper()
	data, err := Encode(value)
	if err != nil {
		t.Fatal(err)
	}
	fields := definitionFields(t, string(data))
	var roles []Role
	if err := json.Unmarshal(fields["roles"], &roles); err != nil {
		t.Fatal(err)
	}
	var members []Member
	if err := json.Unmarshal(fields["members"], &members); err != nil {
		t.Fatal(err)
	}
	if roles == nil || !reflect.DeepEqual(roles, value.Roles) || members == nil || len(members) != 0 {
		t.Fatalf("explicit arrays lost: roles=%#v, members=%#v; want roles=%#v, members=[]", roles, members, value.Roles)
	}
}

func TestTeamCodecControlRecordLimit(t *testing.T) {
	team := teamDefinitionFixture()
	team.Purpose = strings.Repeat("x", task.MaxControlRecordSize)
	if data, err := Encode(team); !errors.Is(err, task.ErrControlRecordTooBig) || data != nil {
		t.Fatalf("oversized Encode = %d bytes, %v; want no data and size error", len(data), err)
	}
	data, err := json.Marshal(team)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(data); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized Decode error = %v, want size error", err)
	}
}

func TestTeamCloneOwnsMutableValues(t *testing.T) {
	original := teamDefinitionFixture()
	clone := original.Clone()
	if !reflect.DeepEqual(clone, teamDefinitionFixture()) {
		t.Fatalf("clone changed definition values: %+v", clone)
	}
	clone.Roles[0].Responsibility = "Changed clone responsibility."
	clone.Members[0].Name = "Changed clone member"
	clone.Members[0].RoleIDs[0] = "reviewer"
	clone.Members[0].ExecutionDefaults.Model = "changed-clone-model"
	if !reflect.DeepEqual(original, teamDefinitionFixture()) {
		t.Fatalf("clone mutation changed original: %+v", original)
	}
	original.Roles[1].Name = "Changed original role"
	original.Members[0].RoleIDs[1] = "author"
	original.Members[0].ExecutionDefaults.Effort = "changed-original-effort"
	if clone.Roles[1].Name != "Author" || clone.Members[0].RoleIDs[1] != "reviewer" || clone.Members[0].ExecutionDefaults.Effort != "experimental" {
		t.Fatalf("original mutation changed clone: %+v", clone)
	}
}

func TestTeamClonePreservesRequiredArrayPresence(t *testing.T) {
	for _, field := range []string{"roles", "members"} {
		t.Run(field, func(t *testing.T) {
			original := Team{SchemaVersion: 1, ID: "empty-team", Name: "Empty team", Purpose: "Await members.", Roles: []Role{}, Members: []Member{}}
			if field == "roles" {
				original.Roles = nil
			} else {
				original.Members = nil
			}
			if err := Validate(original); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("missing array control error = %v, want ErrInvalidDefinition", err)
			}
			clone := original.Clone()
			if !reflect.DeepEqual(clone, original) {
				t.Errorf("Clone changed required array presence: %#v, want %#v", clone, original)
			}
			if err := Validate(clone); !errors.Is(err, ErrInvalidDefinition) {
				t.Errorf("Clone made a missing required array valid: %v", err)
			}
		})
	}
}
