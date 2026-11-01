package task

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const teamTestCatalogID = "0123456789abcdef0123456789abcdef"

func teamContextFixture(projectPath string) *TeamContext {
	return &TeamContext{
		SchemaVersion: 1,
		Target: TeamTarget{
			Team:     TeamReference{Scope: "project", TeamID: "review-team", Project: &TeamProject{Kind: "git", Path: projectPath}},
			MemberID: "primary", CatalogID: teamTestCatalogID,
		},
		TeamRevision: 7, TeamName: "Review team", TeamPurpose: "Review changes.", MemberName: "Primary",
		Roles: []TeamRoleContext{
			{ID: "reviewer", Name: "Reviewer", Responsibility: "Check correctness."},
			{ID: "author", Name: "Author", Responsibility: "Explain the change."},
		},
		Intent: TeamIntent{Provider: "future-agent:exec", Config: TaskConfig{
			Model: "vendor/model-2027:exact", Effort: "experimental", Permission: "read-only", Budget: "120s",
			ReviewTarget: &ReviewTarget{Kind: ReviewTargetBase, Value: "main"},
		}},
	}
}

func TestTeamIDGrammar(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"a", true},
		{"7", true},
		{"team-7-review", true},
		{strings.Repeat("a", 64), true},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"Review", false},
		{"-review", false},
		{"review-", false},
		{"review--team", false},
		{"review_team", false},
		{"review.team", false},
		{"review/team", false},
		{"review team", false},
		{"review\n", false},
		{"réview", false},
		{"team\x00", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			err := ValidateTeamID(tc.id)
			if tc.valid {
				if err != nil {
					t.Fatalf("valid ID rejected: %v", err)
				}
			} else if !errors.Is(err, ErrInvalidTeamSelection) {
				t.Fatalf("ValidateTeamID error = %v, want ErrInvalidTeamSelection", err)
			}
		})
	}
}

func TestTeamReferenceScopeAndCanonicalProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project with spaces")
	project := &TeamProject{Kind: "git", Path: path}
	cases := []struct {
		name      string
		reference TeamReference
		valid     bool
	}{
		{"user", TeamReference{Scope: "user", TeamID: "same-team"}, true},
		{"git project", TeamReference{Scope: "project", TeamID: "same-team", Project: project}, true},
		{"directory project", TeamReference{Scope: "project", TeamID: "same-team", Project: &TeamProject{Kind: "directory", Path: path}}, true},
		{"user with project", TeamReference{Scope: "user", TeamID: "same-team", Project: project}, false},
		{"missing project", TeamReference{Scope: "project", TeamID: "same-team"}, false},
		{"implicit scope", TeamReference{TeamID: "same-team"}, false},
		{"display scope", TeamReference{Scope: "global", TeamID: "same-team"}, false},
		{"missing team", TeamReference{Scope: "user"}, false},
		{"invalid team", TeamReference{Scope: "user", TeamID: "Same-Team"}, false},
		{"unknown project kind", TeamReference{Scope: "project", TeamID: "same-team", Project: &TeamProject{Kind: "remote", Path: path}}, false},
		{"relative project", TeamReference{Scope: "project", TeamID: "same-team", Project: &TeamProject{Kind: "git", Path: "relative"}}, false},
		{"unclean project", TeamReference{Scope: "project", TeamID: "same-team", Project: &TeamProject{Kind: "git", Path: path + string(filepath.Separator) + "."}}, false},
		{"empty project path", TeamReference{Scope: "project", TeamID: "same-team", Project: &TeamProject{Kind: "git"}}, false},
		{"NUL project path", TeamReference{Scope: "project", TeamID: "same-team", Project: &TeamProject{Kind: "git", Path: path + "\x00"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTeamReference(tc.reference)
			if tc.valid {
				if err != nil {
					t.Fatalf("valid scoped reference rejected: %v", err)
				}
			} else if !errors.Is(err, ErrInvalidTeamSelection) {
				t.Fatalf("ValidateTeamReference error = %v, want ErrInvalidTeamSelection", err)
			}
		})
	}
}

func TestTeamTargetCatalogIdentity(t *testing.T) {
	cases := []struct {
		name, catalog string
		required      bool
		valid         bool
	}{
		{"caller may omit catalog", "", false, true},
		{"snapshot requires catalog", "", true, false},
		{"pinned caller", teamTestCatalogID, false, true},
		{"pinned snapshot", teamTestCatalogID, true, true},
		{"short catalog", "0123", false, false},
		{"uppercase catalog", strings.ToUpper(teamTestCatalogID), true, false},
		{"nonhex catalog", strings.Repeat("g", 32), false, false},
		{"long catalog", teamTestCatalogID + "0", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := TeamTarget{Team: TeamReference{Scope: "user", TeamID: "review-team"}, MemberID: "primary", CatalogID: tc.catalog}
			err := ValidateTeamTarget(target, tc.required)
			if tc.valid {
				if err != nil {
					t.Fatalf("valid target rejected: %v", err)
				}
			} else if !errors.Is(err, ErrInvalidTeamSelection) || !errors.Is(err, ErrInvalidRootID) {
				t.Fatalf("invalid catalog error = %v, want selection and root-ID errors", err)
			}
		})
	}
	for _, memberID := range []string{"", "Primary", "../primary"} {
		t.Run("member/"+memberID, func(t *testing.T) {
			target := TeamTarget{Team: TeamReference{Scope: "user", TeamID: "review-team"}, MemberID: memberID}
			if err := ValidateTeamTarget(target, false); !errors.Is(err, ErrInvalidTeamSelection) {
				t.Fatalf("invalid member error = %v, want ErrInvalidTeamSelection", err)
			}
		})
	}
}

func TestTeamContextValidity(t *testing.T) {
	path := t.TempDir()
	cases := []struct {
		name   string
		mutate func(*TeamContext)
	}{
		{"missing version", func(v *TeamContext) { v.SchemaVersion = 0 }},
		{"future version", func(v *TeamContext) { v.SchemaVersion = 2 }},
		{"zero revision", func(v *TeamContext) { v.TeamRevision = 0 }},
		{"negative revision", func(v *TeamContext) { v.TeamRevision = -1 }},
		{"missing catalog", func(v *TeamContext) { v.Target.CatalogID = "" }},
		{"invalid scope", func(v *TeamContext) { v.Target.Team.Scope = "global" }},
		{"missing roles", func(v *TeamContext) { v.Roles = nil }},
		{"empty roles", func(v *TeamContext) { v.Roles = []TeamRoleContext{} }},
		{"duplicate role", func(v *TeamContext) { v.Roles = append(v.Roles, v.Roles[0]) }},
		{"invalid role ID", func(v *TeamContext) { v.Roles[0].ID = "Reviewer" }},
		{"blank team name", func(v *TeamContext) { v.TeamName = " " }},
		{"blank purpose", func(v *TeamContext) { v.TeamPurpose = "\n" }},
		{"blank member name", func(v *TeamContext) { v.MemberName = " " }},
		{"blank role name", func(v *TeamContext) { v.Roles[0].Name = " " }},
		{"blank responsibility", func(v *TeamContext) { v.Roles[0].Responsibility = " " }},
		{"invalid team name UTF-8", func(v *TeamContext) { v.TeamName = "\xff" }},
		{"invalid purpose UTF-8", func(v *TeamContext) { v.TeamPurpose = "\xff" }},
		{"invalid member name UTF-8", func(v *TeamContext) { v.MemberName = "\xff" }},
		{"invalid role name UTF-8", func(v *TeamContext) { v.Roles[0].Name = "\xff" }},
		{"invalid responsibility UTF-8", func(v *TeamContext) { v.Roles[0].Responsibility = "\xff" }},
		{"provider and auto", func(v *TeamContext) { v.Intent.Auto = true }},
		{"invalid provider", func(v *TeamContext) { v.Intent.Provider = "codex" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := teamContextFixture(path)
			tc.mutate(value)
			if err := ValidateTeamContext(value); !errors.Is(err, ErrInvalidTeamSelection) {
				t.Fatalf("ValidateTeamContext error = %v, want ErrInvalidTeamSelection", err)
			}
		})
	}
	if err := ValidateTeamContext(nil); err != nil {
		t.Fatalf("legacy absence rejected: %v", err)
	}
}

func TestTeamContextPreservesRawIntent(t *testing.T) {
	path := t.TempDir()
	for _, intent := range []TeamIntent{{}, {Auto: true}, teamContextFixture(path).Intent} {
		value := teamContextFixture(path)
		value.Intent = intent
		if err := ValidateTeamContext(value); err != nil {
			t.Fatalf("valid raw intent rejected: %v", err)
		}
		data, err := MarshalCanonical(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded TeamContext
		if err := DecodeStrict(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(&decoded, value) || !reflect.DeepEqual(decoded.Intent, intent) {
			t.Fatalf("snapshot changed raw intent: %+v, want %+v", decoded, value)
		}
	}
}

func TestTeamContextRejectsInvalidRequestedOptions(t *testing.T) {
	for _, config := range []TaskConfig{
		{Model: " padded"},
		{Effort: "high "},
		{Model: "mo\x00del"},
		{Effort: "hi\x00gh"},
		{Permission: "admin"},
		{Budget: "0s"},
		{Budget: "1m", NativeTimeout: "2m"},
		{ReviewTarget: &ReviewTarget{Kind: "unknown"}},
	} {
		value := teamContextFixture(t.TempDir())
		value.Intent.Config = config
		if err := ValidateTeamContext(value); err == nil {
			t.Errorf("invalid raw options accepted: %+v", config)
		}
	}
}

func TestTeamContextStrictJSON(t *testing.T) {
	data, err := MarshalCanonical(teamContextFixture(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema_version", "target", "team_revision", "team_name", "team_purpose", "member_name", "roles", "intent"} {
		t.Run("missing/"+field, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, field)
			invalid, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var decoded TeamContext
			if err := DecodeStrict(invalid, &decoded); err == nil {
				t.Fatal("missing required snapshot field accepted")
			}
		})
	}
}

func TestTeamReferenceAndTargetClonesOwnProjects(t *testing.T) {
	path := t.TempDir()
	original := teamContextFixture(path).Target
	reference := original.Team.Clone()
	target := original.Clone()
	reference.Project.Path = filepath.Join(path, "reference-clone")
	target.Team.Project.Kind = "directory"
	if original.Team.Project.Path != path || original.Team.Project.Kind != "git" {
		t.Fatalf("clones changed original project: %+v", original.Team.Project)
	}
	original.Team.Project.Path = filepath.Join(path, "original")
	if target.Team.Project.Path != path {
		t.Fatalf("original changed target clone project: %+v", target.Team.Project)
	}
	user := TeamReference{Scope: "user", TeamID: "review-team"}
	if !reflect.DeepEqual(user.Clone(), user) {
		t.Fatal("cloning user reference changed its value")
	}
}

func TestTeamContextCloneOwnsSnapshot(t *testing.T) {
	path := t.TempDir()
	original := teamContextFixture(path)
	clone := original.Clone()
	if !reflect.DeepEqual(clone, teamContextFixture(path)) {
		t.Fatalf("clone changed snapshot values: %+v", clone)
	}
	clone.Target.Team.Project.Path = filepath.Join(path, "clone")
	clone.Roles[0].Name = "Changed clone role"
	clone.Intent.Config.ReviewTarget.Value = "clone-branch"
	if !reflect.DeepEqual(original, teamContextFixture(path)) {
		t.Fatalf("clone mutation changed original: %+v", original)
	}
	original.Target.Team.Project.Kind = "directory"
	original.Roles[1].Responsibility = "Changed original responsibility."
	original.Intent.Config.ReviewTarget.Kind = ReviewTargetUncommitted
	if clone.Target.Team.Project.Kind != "git" || clone.Roles[1].Responsibility != "Explain the change." || clone.Intent.Config.ReviewTarget.Kind != ReviewTargetBase {
		t.Fatalf("original mutation changed clone: %+v", clone)
	}
	var absent *TeamContext
	if absent.Clone() != nil {
		t.Fatal("cloning absent context must preserve absence")
	}
}
