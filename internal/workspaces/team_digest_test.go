package workspaces

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const teamDigestTaskID = "fedcba9876543210fedcba9876543210"

func teamDigestRequest() DispatchRequest {
	return DispatchRequest{
		Provider: "fixture:test", Base: "main", Config: task.TaskConfig{Permission: "read-only", Budget: "1m0s"},
		Team:       &task.TeamTarget{Team: task.TeamReference{Scope: "user", TeamID: "review-team"}, MemberID: "primary", CatalogID: "0123456789abcdef0123456789abcdef"},
		TeamIntent: &task.TeamIntent{Config: task.TaskConfig{Permission: "read-only"}},
	}
}

func digestTeamRequest(t *testing.T, request DispatchRequest) string {
	t.Helper()
	digest, err := DispatchDigest(request, teamDigestTaskID, "/work/repository", []byte("hello"), time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestTeamDigestNoTargetRetainsLegacyByteIdentity(t *testing.T) {
	// Hand-written pre-Team identity bytes; the brief hash is SHA-256("hello").
	const legacy = `{"task_id":"fedcba9876543210fedcba9876543210","provider":"fixture:test","auto":false,"source_cwd":"/work/repository","base":"main","config":{"permission":"read-only","budget":"1m0s"},"brief_sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","workspace_budget_nanos":60000000000}` + "\n"
	request := teamDigestRequest()
	request.Team, request.TeamIntent = nil, nil
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
	if got := digestTeamRequest(t, request); got != want {
		t.Fatalf("no-Team digest = %s, want frozen legacy digest %s", got, want)
	}
}

func TestTeamDigestIgnoresChangedEffectiveMemberDefaults(t *testing.T) {
	original := teamDigestRequest()
	before := digestTeamRequest(t, original)
	changed := teamDigestRequest()
	changed.Provider = "future-agent:exec"
	changed.Auto = true
	changed.Config = task.TaskConfig{Permission: "read-only", Budget: "9m0s", Model: "new-default-model", Effort: "new-default-effort"}
	if after := digestTeamRequest(t, changed); after != before {
		t.Fatalf("effective defaults changed raw request identity: %s != %s", after, before)
	}
}

func TestTeamDigestBindsRawTargetsAndOptions(t *testing.T) {
	changes := []struct {
		name string
		edit func(*DispatchRequest)
	}{
		{"team", func(r *DispatchRequest) { r.Team.Team.TeamID = "other-team" }},
		{"member", func(r *DispatchRequest) { r.Team.MemberID = "secondary" }},
		{"catalog", func(r *DispatchRequest) { r.Team.CatalogID = "fedcba9876543210fedcba9876543210" }},
		{"project", func(r *DispatchRequest) {
			r.Team.Team.Scope, r.Team.Team.Project = "project", &task.TeamProject{Kind: "git", Path: "/work/repository/.git"}
		}},
		{"provider", func(r *DispatchRequest) { r.TeamIntent.Provider = "fixture:test" }},
		{"auto", func(r *DispatchRequest) { r.TeamIntent.Auto = true }},
		{"model", func(r *DispatchRequest) { r.TeamIntent.Config.Model = "vendor/exact-model" }},
		{"effort", func(r *DispatchRequest) { r.TeamIntent.Config.Effort = "experimental" }},
		{"permission", func(r *DispatchRequest) { r.TeamIntent.Config.Permission = "workspace-write" }},
		{"budget", func(r *DispatchRequest) { r.TeamIntent.Config.Budget = "1m" }},
		{"native timeout", func(r *DispatchRequest) { r.TeamIntent.Config.NativeTimeout = "30s" }},
		{"interactive", func(r *DispatchRequest) { r.TeamIntent.Config.Interactive = true }},
		{"review target", func(r *DispatchRequest) {
			r.TeamIntent.Config.ReviewTarget = &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"}
		}},
	}
	before := digestTeamRequest(t, teamDigestRequest())
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			request := teamDigestRequest()
			change.edit(&request)
			if after := digestTeamRequest(t, request); after == before {
				t.Fatal("different raw requested intent has the same digest")
			}
		})
	}
}
