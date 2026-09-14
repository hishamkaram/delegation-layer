package codex

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProjectInventoryStopsAtNativeGitBoundary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	workspace := filepath.Join(repository, "nested")
	if err = os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(repository, ".git")
	if err = os.Mkdir(git, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := policyFiles(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(workspace, ".codex/config.toml"), filepath.Join(repository, ".codex/config.toml")} {
		if !slices.Contains(paths, path) {
			t.Fatalf("missing project layer %s", path)
		}
	}
	if slices.Contains(paths, filepath.Join(root, ".codex/config.toml")) {
		t.Fatal("ignored ancestor user config entered project inventory")
	}
	if err = os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: ../external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nearest, err := projectRoot(workspace)
	if err != nil || nearest != workspace {
		t.Fatalf("worktree marker mismatch: %s %v", nearest, err)
	}
}

func TestProjectMarkerRejectsAliasAndSkipsIncompleteGitDirectory(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, ".git")
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	valid, err := gitRootMarker(marker)
	if err != nil || valid {
		t.Fatalf("directory without HEAD counted: %v %v", valid, err)
	}
	link := filepath.Join(root, "alias")
	if err = os.Symlink(marker, link); err != nil {
		t.Fatal(err)
	}
	if _, err = gitRootMarker(link); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("alias accepted: %v", err)
	}
}

func TestEmptyConfigurationDistinguishesAbsentPresentAndUnsupported(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.toml")
	absent, err := emptyConfigSources([]string{path})
	if err != nil || len(absent) != 1 || absent[0].Present || absent[0].SHA256 != "" {
		t.Fatalf("absent: %+v %v", absent, err)
	}
	if err = os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	present, err := emptyConfigSources([]string{path})
	if err != nil || len(present) != 1 || !present[0].Present || present[0].SHA256 != task.ComputeSHA256(nil) {
		t.Fatalf("present: %+v %v", present, err)
	}
	for _, data := range []string{"[mcp_servers.example]\ncommand='example'", "[features]\nhooks=true", "sandbox_mode='danger-full-access'", "# nonempty config requires separate support"} {
		if err = os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err = emptyConfigSources([]string{path}); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("accepted unsupported control source: %v", err)
		}
	}
}

func TestPolicyDigestBindsNonSecretSourcesAndPlacement(t *testing.T) {
	request := profileRequest()
	environment := profileEnvironment{WritableRoots: []string{"/runtime"}}
	sources := []task.PolicySourceDigest{{Path: "/config.toml", Kind: "empty-native-config"}}
	first, err := sealPolicy(request, environment, sources)
	if err != nil {
		t.Fatal(err)
	}
	sources[0].Present, sources[0].SHA256 = true, task.ComputeSHA256(nil)
	changed, err := sealPolicy(request, environment, sources)
	if err != nil || changed.Digest == first.Digest {
		t.Fatalf("configuration presence drift escaped digest: %v", err)
	}
	environment.WritableRoots = []string{"/other-runtime"}
	placement, err := sealPolicy(request, environment, sources)
	if err != nil || placement.Digest == changed.Digest {
		t.Fatalf("runtime placement drift escaped digest: %v", err)
	}
	request.CanonicalCwd = "/other-workspace"
	workspace, err := sealPolicy(request, environment, sources)
	if err != nil || workspace.Digest == placement.Digest {
		t.Fatalf("workspace drift escaped digest: %v", err)
	}
}

func TestManagedInspectionFailureStopsPolicyPreparation(t *testing.T) {
	failure := errors.New("injected preference API failure")
	_, err := effectivePolicy(profileRequest(), profileEnvironment{}, time.Now(), func() ([]task.PolicySourceDigest, error) { return nil, failure })
	if !errors.Is(err, failure) {
		t.Fatalf("managed lookup failure lost: %v", err)
	}
}
