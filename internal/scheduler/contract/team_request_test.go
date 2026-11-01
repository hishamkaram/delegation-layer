package contract

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func scheduledTeamRequest() Request {
	return Request{
		CanonicalCwd: "/work/repository",
		Config:       task.TaskConfig{Model: "vendor/exact-model", ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"}},
		Team: &task.TeamTarget{
			Team:     task.TeamReference{Scope: "project", TeamID: "review-team", Project: &task.TeamProject{Kind: "git", Path: "/work/repository/.git"}},
			MemberID: "primary", CatalogID: "0123456789abcdef0123456789abcdef",
		},
	}
}

func TestTeamRequestNoTargetRetainsLegacyBytesAndDigest(t *testing.T) {
	// Frozen pre-Team request bytes and the existing independent definition
	// digest pin compatibility without deriving expectations from new output.
	const legacy = `{"provider":"codex:exec","canonical_cwd":"/work/repository","config":{"model":"gpt","permission":"read-only","review_target":{"kind":"base","value":"main"}}}` + "\n"
	encoded, err := MarshalRequest(testRequest())
	if err != nil || !bytes.Equal(encoded, []byte(legacy)) {
		t.Fatalf("legacy request = %q, %v; want %q", encoded, err, legacy)
	}
	decoded, err := DecodeRequest([]byte(legacy))
	if err != nil || !sameRequest(decoded, testRequest()) || decoded.Team != nil {
		t.Fatalf("legacy decode = %+v, %v", decoded, err)
	}
	for _, definition := range []Definition{testDefinition(), testDefinition().Clone()} {
		digests, err := DefinitionDigests(definition)
		if err != nil || digests.DefinitionSHA256 != goldenDefinitionSHA256 {
			t.Fatalf("legacy definition digest = %+v, %v; want %s", digests, err, goldenDefinitionSHA256)
		}
	}
}

func TestTeamRequestRoundTripKeepsRawChoicesAndCatalog(t *testing.T) {
	for _, selection := range []Request{scheduledTeamRequest(), {Auto: true, CanonicalCwd: "/work/repository", Team: scheduledTeamRequest().Team}} {
		encoded, err := MarshalRequest(selection)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeRequest(encoded)
		if err != nil || !reflect.DeepEqual(decoded, selection) || !sameRequest(decoded, selection) {
			t.Fatalf("Team round trip = %+v, %v; want %+v", decoded, err, selection)
		}
		if decoded.Provider != "" || decoded.Config.Permission != "" || decoded.Config.Budget != "" || decoded.Config.Effort != "" {
			t.Fatalf("saved request acquired resolved defaults: %+v", decoded)
		}
	}
}

func TestTeamRequestCloneOwnsTargetProjectAndOptions(t *testing.T) {
	original := scheduledTeamRequest()
	clone := original.clone()
	if !reflect.DeepEqual(original, clone) || !sameRequest(original, clone) {
		t.Fatal("equal cloned values were compared by pointer identity")
	}
	clone.Team.MemberID = "secondary"
	clone.Team.Team.Project.Path = "/work/another/.git"
	clone.Config.ReviewTarget.Value = "branch"
	if !reflect.DeepEqual(original, scheduledTeamRequest()) || sameRequest(original, clone) {
		t.Fatal("clone aliases the original or target changes compare equal")
	}
	definition := testDefinition()
	definition.Request = scheduledTeamRequest()
	copy := definition.Clone()
	copy.Request.Team.CatalogID = "fedcba9876543210fedcba9876543210"
	copy.Request.Team.Team.Project.Kind = "directory"
	if !reflect.DeepEqual(definition.Request, scheduledTeamRequest()) {
		t.Fatal("Definition.Clone shares Team target memory")
	}
}

func TestTeamRequestEqualityAndDigestBindRequestedTarget(t *testing.T) {
	changes := []struct {
		name string
		edit func(*Request)
	}{
		{"team", func(r *Request) { r.Team.Team.TeamID = "other-team" }},
		{"member", func(r *Request) { r.Team.MemberID = "secondary" }},
		{"catalog", func(r *Request) { r.Team.CatalogID = "fedcba9876543210fedcba9876543210" }},
		{"project", func(r *Request) { r.Team.Team.Project.Path = "/work/another/.git" }},
		{"scope", func(r *Request) { r.Team.Team.Scope, r.Team.Team.Project = "user", nil }},
		{"provider", func(r *Request) { r.Provider = "future-agent:exec" }},
		{"auto", func(r *Request) { r.Auto = true }},
		{"model", func(r *Request) { r.Config.Model = "other/exact-model" }},
		{"effort", func(r *Request) { r.Config.Effort = "experimental" }},
	}
	original := testDefinition()
	original.Request = scheduledTeamRequest()
	before, err := DefinitionDigests(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			changed := original.Clone()
			change.edit(&changed.Request)
			after, err := DefinitionDigests(changed)
			if err != nil || after.DefinitionSHA256 == before.DefinitionSHA256 || sameRequest(original.Request, changed.Request) {
				t.Fatalf("changed requested identity compared equal: %+v, %v", after, err)
			}
		})
	}
}

func TestTeamRequestRejectsUnpinnedOrInvalidSelection(t *testing.T) {
	for _, change := range []func(*Request){
		func(r *Request) { r.Team.CatalogID = "" },
		func(r *Request) { r.Team.MemberID = "" },
		func(r *Request) { r.Team.Team.Project = nil },
		func(r *Request) { r.Provider, r.Auto = "codex:exec", true },
	} {
		request := scheduledTeamRequest()
		change(&request)
		if encoded, err := MarshalRequest(request); err == nil || encoded != nil {
			t.Errorf("invalid schedule selection encoded: %q, %v", encoded, err)
		}
	}
}
