package contract

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const selectionTestCatalogID = "0123456789abcdef0123456789abcdef"

func selectionIntentFixture(provider, model, effort string) task.TeamIntent {
	return task.TeamIntent{Provider: provider, Config: task.TaskConfig{
		Model: model, Effort: effort, Permission: "workspace-write", Budget: "120s", NativeTimeout: "30s", Interactive: true,
		ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"},
	}}
}

func TestMergeExecutionProviderPrecedence(t *testing.T) {
	cases := []struct {
		name, provider, model, effort string
		wantProvider, wantModel       string
		wantEffort                    string
	}{
		{"inherit all", "", "", "", "codex:exec", "inherited-model", "inherited-effort"},
		{"explicit model", "", "vendor/exact-model", "", "codex:exec", "vendor/exact-model", "inherited-effort"},
		{"explicit effort", "", "", "experimental", "codex:exec", "inherited-model", "experimental"},
		{"same provider inherits", "codex:exec", "", "", "codex:exec", "inherited-model", "inherited-effort"},
		{"same provider overrides", "codex:exec", "vendor/exact-model", "experimental", "codex:exec", "vendor/exact-model", "experimental"},
		{"changed provider clears inherited options", "opencode:run", "", "", "opencode:run", "", ""},
		{"changed provider explicit model", "opencode:run", "vendor/exact-model", "", "opencode:run", "vendor/exact-model", ""},
		{"changed provider explicit effort", "opencode:run", "", "experimental", "opencode:run", "", "experimental"},
		{"changed provider explicit options", "future-agent:exec", "vendor/exact-model", "experimental", "future-agent:exec", "vendor/exact-model", "experimental"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defaults := &ExecutionDefaults{Provider: "codex:exec", Model: "inherited-model", Effort: "inherited-effort"}
			requested := selectionIntentFixture(tc.provider, tc.model, tc.effort)
			got, err := MergeExecution(defaults, requested)
			if err != nil {
				t.Fatal(err)
			}
			wantConfig := selectionIntentFixture("", tc.wantModel, tc.wantEffort).Config
			want := Execution{Provider: tc.wantProvider, Config: wantConfig}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("MergeExecution = %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(requested, selectionIntentFixture(tc.provider, tc.model, tc.effort)) {
				t.Fatalf("merge mutated caller intent: %+v", requested)
			}
			if *defaults != (ExecutionDefaults{Provider: "codex:exec", Model: "inherited-model", Effort: "inherited-effort"}) {
				t.Fatalf("merge mutated member defaults: %+v", defaults)
			}
		})
	}
}

func TestMergeExecutionDoesNotSupplyTaskAuthority(t *testing.T) {
	defaults := &ExecutionDefaults{Provider: "future-agent:exec", Model: "exact-model", Effort: "experimental"}
	got, err := MergeExecution(defaults, task.TeamIntent{})
	if err != nil {
		t.Fatal(err)
	}
	want := Execution{Provider: "future-agent:exec", Config: task.TaskConfig{Model: "exact-model", Effort: "experimental"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge supplied unrequested options: %+v, want %+v", got, want)
	}
}

func TestMergeExecutionUnboundMemberNeedsDestination(t *testing.T) {
	for _, requested := range []task.TeamIntent{
		{}, {Config: task.TaskConfig{Model: "exact-model"}}, {Config: task.TaskConfig{Effort: "experimental"}},
	} {
		if got, err := MergeExecution(nil, requested); err == nil || !reflect.DeepEqual(got, Execution{}) {
			t.Errorf("unbound member without destination = %+v, %v; want no execution and an error", got, err)
		}
	}
	for _, provider := range []string{"codex:exec", "future-agent:exec"} {
		requested := selectionIntentFixture(provider, "vendor/exact-model", "experimental")
		got, err := MergeExecution(nil, requested)
		want := Execution{Provider: provider, Config: selectionIntentFixture(provider, "vendor/exact-model", "experimental").Config}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("explicit unbound destination = %+v, %v; want %+v", got, err, want)
		}
	}
	requested := selectionIntentFixture("", "vendor/exact-model", "experimental")
	requested.Auto = true
	got, err := MergeExecution(nil, requested)
	want := Execution{Auto: true, Config: selectionIntentFixture("", "vendor/exact-model", "experimental").Config}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit auto = %+v, %v; want %+v", got, err, want)
	}
}

func TestMergeExecutionRejectsAutoConflicts(t *testing.T) {
	cases := []struct {
		name      string
		defaults  *ExecutionDefaults
		requested task.TeamIntent
	}{
		{"explicit provider", nil, task.TeamIntent{Auto: true, Provider: "codex:exec"}},
		{"member provider", &ExecutionDefaults{Provider: "codex:exec"}, task.TeamIntent{Auto: true}},
		{"same explicit and member provider", &ExecutionDefaults{Provider: "codex:exec"}, task.TeamIntent{Auto: true, Provider: "codex:exec"}},
		{"changed explicit provider", &ExecutionDefaults{Provider: "codex:exec"}, task.TeamIntent{Auto: true, Provider: "opencode:run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := MergeExecution(tc.defaults, tc.requested); err == nil || !reflect.DeepEqual(got, Execution{}) {
				t.Fatalf("conflicting auto = %+v, %v; want no execution and an error", got, err)
			}
		})
	}
}

func TestMergeExecutionRejectsInvalidCallerOptions(t *testing.T) {
	for _, config := range []task.TaskConfig{
		{Model: " padded"},
		{Effort: "high "},
		{Model: "mo\x00del"},
		{Effort: "hi\x00gh"},
		{Permission: "admin"},
		{Budget: "0s"},
		{Budget: "1m", NativeTimeout: "2m"},
		{ReviewTarget: &task.ReviewTarget{Kind: "unknown"}},
	} {
		requested := task.TeamIntent{Provider: "codex:exec", Config: config}
		if got, err := MergeExecution(nil, requested); err == nil || !reflect.DeepEqual(got, Execution{}) {
			t.Errorf("invalid caller options = %+v, %v; want no execution and an error", got, err)
		}
	}
}

func resolvedMemberFixture(projectPath string) ResolvedMember {
	definition := teamDefinitionFixture()
	definition.Roles = append(definition.Roles, Role{ID: "unselected", Name: "Unselected", Responsibility: "Another member's role."})
	return ResolvedMember{
		CatalogID: selectionTestCatalogID,
		Record: Record{
			Reference: task.TeamReference{Scope: "project", TeamID: "review-team", Project: &task.TeamProject{Kind: "git", Path: projectPath}},
			Revision:  7, Definition: definition,
		},
		Member: definition.Members[0],
	}
}

func selectionContextFixture(projectPath string) *task.TeamContext {
	return &task.TeamContext{
		SchemaVersion: 1,
		Target: task.TeamTarget{
			Team:     task.TeamReference{Scope: "project", TeamID: "review-team", Project: &task.TeamProject{Kind: "git", Path: projectPath}},
			MemberID: "primary", CatalogID: selectionTestCatalogID,
		},
		TeamRevision: 7, TeamName: "Review team", TeamPurpose: "Review changes — carefully.", MemberName: "Primary",
		Roles: []task.TeamRoleContext{
			{ID: "author", Name: "Author", Responsibility: "Explain the change."},
			{ID: "reviewer", Name: "Reviewer", Responsibility: "Check correctness."},
		},
		Intent: selectionIntentFixture("", "", ""),
	}
}

func TestSelectionContextCapturesSelectedRolesAndRawIntent(t *testing.T) {
	path := t.TempDir()
	resolved := resolvedMemberFixture(path)
	intent := selectionIntentFixture("", "", "")
	got, err := SelectionContext(resolved, intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, selectionContextFixture(path)) {
		t.Fatalf("selection snapshot = %+v, want %+v", got, selectionContextFixture(path))
	}
	if !reflect.DeepEqual(resolved, resolvedMemberFixture(path)) || !reflect.DeepEqual(intent, selectionIntentFixture("", "", "")) {
		t.Fatal("selection modified its inputs")
	}
	resolved.Record.Reference.Project.Path = filepath.Join(path, "moved")
	resolved.Record.Revision++
	resolved.Record.Definition.Name = "Updated team"
	resolved.Record.Definition.Purpose = "Updated purpose."
	resolved.Record.Definition.Roles[0].Responsibility = "Updated responsibility."
	resolved.Member.Name = "Updated member"
	resolved.Member.RoleIDs[0] = "reviewer"
	resolved.Member.ExecutionDefaults.Model = "updated-model"
	intent.Config.ReviewTarget.Value = "updated-branch"
	if !reflect.DeepEqual(got, selectionContextFixture(path)) {
		t.Fatalf("mutable catalog or caller values changed admitted snapshot: %+v", got)
	}
}

func TestSelectionContextRejectsInvalidSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ResolvedMember)
	}{
		{"missing catalog", func(v *ResolvedMember) { v.CatalogID = "" }},
		{"invalid catalog", func(v *ResolvedMember) { v.CatalogID = "other-catalog" }},
		{"missing revision", func(v *ResolvedMember) { v.Record.Revision = 0 }},
		{"missing project", func(v *ResolvedMember) { v.Record.Reference.Project = nil }},
		{"blank team name", func(v *ResolvedMember) { v.Record.Definition.Name = " " }},
		{"blank member name", func(v *ResolvedMember) { v.Member.Name = " " }},
		{"empty roles", func(v *ResolvedMember) { v.Member.RoleIDs = []string{} }},
		{"unknown role", func(v *ResolvedMember) { v.Member.RoleIDs = []string{"missing"} }},
		{"unknown among valid roles", func(v *ResolvedMember) { v.Member.RoleIDs = []string{"reviewer", "missing"} }},
		{"repeated role", func(v *ResolvedMember) { v.Member.RoleIDs = []string{"reviewer", "reviewer"} }},
		{"blank responsibility", func(v *ResolvedMember) { v.Record.Definition.Roles[0].Responsibility = " " }},
	}
	path := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolvedMemberFixture(path)
			tc.mutate(&resolved)
			got, err := SelectionContext(resolved, task.TeamIntent{})
			if got != nil || !errors.Is(err, task.ErrInvalidTeamSelection) {
				t.Fatalf("SelectionContext = %+v, %v; want no context and invalid selection", got, err)
			}
		})
	}
}
