package teams

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestScopeUniquenessAndListing(t *testing.T) {
	catalog := testCatalog(t)
	project := task.TeamProject{Kind: "directory", Path: t.TempDir()}
	other := task.TeamProject{Kind: "directory", Path: t.TempDir()}
	user := userRef("review")
	local := task.TeamReference{Scope: "project", TeamID: "review", Project: &project}
	foreign := task.TeamReference{Scope: "project", TeamID: "review", Project: &other}
	registerTeam(t, catalog, user)
	registerTeam(t, catalog, local)
	registerTeam(t, catalog, foreign)
	listing, err := catalog.List(t.Context(), &project)
	if err != nil || len(listing.Records) != 2 || len(listing.Diagnostics) != 0 {
		t.Fatalf("same-name scopes/list: %+v %v", listing, err)
	}
	listing, err = catalog.List(t.Context(), nil)
	if err != nil || len(listing.Records) != 1 || listing.Records[0].Reference.Scope != "user" {
		t.Fatalf("user-only list: %+v %v", listing, err)
	}
	if result, defaultErr := catalog.Default(t.Context(), project); defaultErr != nil || result.Status != "unset" {
		t.Fatalf("registration assigned default: %+v %v", result, defaultErr)
	}
	if setErr := catalog.SetDefault(t.Context(), project, foreign); !errors.Is(setErr, ErrProjectMismatch) {
		t.Fatalf("foreign project accepted: %v", setErr)
	}
	if setErr := catalog.SetDefault(t.Context(), project, userRef("missing")); !errors.Is(setErr, ErrNotFound) {
		t.Fatalf("missing default target accepted: %v", setErr)
	}
	if result, defaultErr := catalog.Default(t.Context(), other); defaultErr != nil || result.Status != "unset" {
		t.Fatalf("registration leaked a preference: %+v %v", result, defaultErr)
	}
}

func TestDeletedProjectDefaultStaysStale(t *testing.T) {
	catalog := testCatalog(t)
	project := task.TeamProject{Kind: "directory", Path: t.TempDir()}
	user := userRef("review")
	local := task.TeamReference{Scope: "project", TeamID: "review", Project: &project}
	registerTeam(t, catalog, user)
	registerTeam(t, catalog, local)
	if err := catalog.SetDefault(t.Context(), project, local); err != nil {
		t.Fatal(err)
	}
	if result, err := catalog.Default(t.Context(), project); err != nil || result.Status != "resolved" || result.Record.Reference.Scope != "project" {
		t.Fatalf("local default: %+v %v", result, err)
	}
	if _, err := catalog.Delete(t.Context(), local, 1); err != nil {
		t.Fatal(err)
	}
	result, err := New(catalog.path, nil).Default(t.Context(), project)
	if err != nil || result.Status != "stale" || result.Reference.Scope != "project" || result.Record != nil {
		t.Fatalf("deleted default reassigned: %+v %v", result, err)
	}
}

func TestProjectDefaultReadsLatestDefinitionAndClearsIdempotently(t *testing.T) {
	catalog := testCatalog(t)
	project := task.TeamProject{Kind: "directory", Path: t.TempDir()}
	user := userRef("review")
	registerTeam(t, catalog, user)
	if err := catalog.SetDefault(t.Context(), project, user); err != nil {
		t.Fatal(err)
	}
	updated := testTeam("review")
	updated.Purpose = "New live definition"
	if _, _, err := catalog.Update(t.Context(), user, 1, updated); err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Default(t.Context(), project)
	if err != nil || result.Record == nil || result.Record.Revision != 2 || result.Record.Definition.Purpose != updated.Purpose {
		t.Fatalf("default copied stale settings: %+v %v", result, err)
	}
	for range 2 {
		if err := catalog.ClearDefault(t.Context(), project); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := catalog.Default(t.Context(), project); err != nil || result.Status != "unset" {
		t.Fatalf("clear default: %+v %v", result, err)
	}
}

func TestInvalidProjectRequestsDoNotCreateCatalog(t *testing.T) {
	catalog := testCatalog(t)
	invalid := task.TeamProject{Kind: "directory", Path: "relative/path"}
	if err := catalog.SetDefault(t.Context(), invalid, userRef("review")); !errors.Is(err, ErrProjectContext) {
		t.Fatalf("set invalid project: %v", err)
	}
	if err := catalog.ClearDefault(t.Context(), invalid); !errors.Is(err, ErrProjectContext) {
		t.Fatalf("clear invalid project: %v", err)
	}
	if _, err := catalog.Default(t.Context(), invalid); !errors.Is(err, ErrProjectContext) {
		t.Fatalf("read invalid project: %v", err)
	}
	if _, err := catalog.List(t.Context(), &invalid); !errors.Is(err, ErrProjectContext) {
		t.Fatalf("list invalid project: %v", err)
	}
	assertAbsent(t, filepath.Dir(catalog.path))
}
