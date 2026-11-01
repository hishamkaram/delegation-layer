package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

func teamsProjectFixture(t *testing.T) (Dependencies, string, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("native Git is required for project resolution:", err)
	}
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "nested")
	if err = os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	deps := teamsCommandDependencies(t)
	deps.Environment = []string{"PATH=" + filepath.Dir(git), "HOME=" + t.TempDir(), "GIT_CEILING_DIRECTORIES=" + filepath.Dir(root), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	return deps, cwd, root
}

func runTeamsDefaultForTest(t *testing.T, deps Dependencies, cwd, root, action string, flags ...string) (int, TeamsResponse) {
	t.Helper()
	argv := []string{"default", action, "--cwd", cwd, "--project-root", root}
	return runTeamsForTest(t, deps, "", append(argv, flags...)...)
}

func TestTeamsCLIDefaultUnsetResolvedStaleAndClear(t *testing.T) {
	deps, cwd, root := teamsProjectFixture(t)
	exit, response := runTeamsDefaultForTest(t, deps, cwd, root, "show")
	requireTeamsDefault(t, exit, response, "unset")
	if _, err := os.Lstat(filepath.Dir(deps.TeamCatalogPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default show initialized catalog: %v", err)
	}
	exit, response = runTeamsForTest(t, deps, teamsDefinitionJSON, "register", "--scope", "user", "--definition", "-")
	requireTeamsMutation(t, exit, response, "register", 1)
	exit, response = runTeamsDefaultForTest(t, deps, cwd, root, "set", "--team-scope", "user", "--team", "review")
	requireTeamsMutation(t, exit, response, "default_set", 0)
	exit, response = runTeamsDefaultForTest(t, deps, cwd, root, "show")
	requireTeamsDefault(t, exit, response, "resolved")
	exit, response = runTeamsForTest(t, deps, "", "delete", "--scope", "user", "--team", "review", "--expected-revision", "1")
	requireTeamsMutation(t, exit, response, "delete", 2)
	exit, response = runTeamsDefaultForTest(t, deps, cwd, root, "show")
	requireTeamsDefault(t, exit, response, "stale")
	if response.Default.Reference == nil || len(response.Diagnostics) != 1 || response.Status != "partial" {
		t.Fatalf("stale: exit=%d response=%+v", exit, response)
	}
	exit, response = runTeamsDefaultForTest(t, deps, cwd, root, "clear")
	requireTeamsMutation(t, exit, response, "default_clear", 0)
	exit, response = runTeamsDefaultForTest(t, deps, cwd, root, "set", "--team-scope", "user", "--team", "missing")
	if exit != 2 || response.Failure == nil || response.Failure.Code != "not_found" {
		t.Fatalf("invalid default: exit=%d response=%+v", exit, response)
	}
	exit, response = runTeamsDefaultForTest(t, deps, cwd, root, "show")
	requireTeamsDefault(t, exit, response, "unset")
}

func requireTeamsDefault(t *testing.T, exit int, response TeamsResponse, state string) {
	t.Helper()
	if exit != 0 || response.Default == nil || response.Default.State != state || response.Teams != nil {
		t.Fatalf("default %s: exit=%d response=%+v", state, exit, response)
	}
	if state == "resolved" && (response.Default.Team == nil || response.Default.Team.Revision != 1) {
		t.Fatalf("default lost resolved Team: %+v", response.Default)
	}
}

func registerTeamsScopesForTest(t *testing.T) (Dependencies, string, string) {
	t.Helper()
	deps, cwd, root := teamsProjectFixture(t)
	if exit, response := runTeamsForTest(t, deps, teamsDefinitionJSON, "register", "--scope", "user", "--definition", "-"); exit != 0 {
		t.Fatalf("register user: %+v", response)
	}
	definition := strings.Replace(teamsDefinitionJSON, `"name":"Review"`, `"name":"Project Review"`, 1)
	exit, response := runTeamsForTest(t, deps, definition, "register", "--scope", "project", "--cwd", cwd, "--project-root", root, "--definition", "-")
	if exit != 0 {
		t.Fatalf("register project: exit=%d response=%+v", exit, response)
	}
	return deps, cwd, root
}

func TestTeamsCLIExplicitProjectListKeepsSameNameScopesSeparate(t *testing.T) {
	deps, cwd, root := registerTeamsScopesForTest(t)
	exit, response := runTeamsForTest(t, deps, "", "list", "--cwd", cwd, "--project-root", root)
	requireTeamsPage(t, exit, response, []string{"review", "review"}, "")
	if (*response.Teams)[0].Reference.Scope != "project" || (*response.Teams)[1].Reference.Scope != "user" {
		t.Fatalf("mixed list: exit=%d response=%+v", exit, response)
	}
	exit, response = runTeamsForTest(t, deps, "", "show", "--scope", "project", "--team", "review", "--cwd", cwd, "--project-root", root)
	if exit != 0 || response.Team == nil || response.Team.Definition.Name != "Project Review" {
		t.Fatalf("scoped show: exit=%d response=%+v", exit, response)
	}
	exit, response = runTeamsForTest(t, deps, "", "list")
	requireTeamsPage(t, exit, response, []string{"review"}, "")
	if (*response.Teams)[0].Reference.Scope != "user" {
		t.Fatalf("implicit project context: exit=%d response=%+v", exit, response)
	}
}

func TestTeamsCLIUnavailableProjectKeepsUserTeams(t *testing.T) {
	deps, cwd, root := registerTeamsScopesForTest(t)
	deps.Environment = []string{"PATH=" + t.TempDir()}
	exit, response := runTeamsForTest(t, deps, "", "list", "--cwd", cwd, "--project-root", root)
	if exit != 0 || response.Status != "partial" || response.Teams == nil || len(*response.Teams) != 1 || len(response.Diagnostics) != 1 {
		t.Fatalf("Git unavailable hid usable user Teams: exit=%d response=%+v", exit, response)
	}
	exit, response = runTeamsDefaultForTest(t, deps, cwd, root, "show")
	if exit != 1 || response.Failure == nil {
		t.Fatalf("Git unavailable treated as non-Git: exit=%d response=%+v", exit, response)
	}
}
