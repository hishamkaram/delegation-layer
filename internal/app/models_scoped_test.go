package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type scopedModelFixture struct {
	store     *taskdir.Store
	rootToken string
	workspace string
	path      string
	tokenFile string
}

func newScopedModelFixture(t *testing.T, store *taskdir.Store) scopedModelFixture {
	t.Helper()
	if store == nil {
		var err error
		store, err = taskdir.InitStore(filepath.Join(t.TempDir(), "native"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := store.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err := store.EnableWorkspaceConnectorContext(t.Context(), true, true); err != nil {
		t.Fatal(err)
	}
	root, err := store.RootTokenContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(workspace, "project")
	if err = os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := taskdir.ObservePathProjectContext(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	token, err := taskdir.ProjectToken(identity)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "workspace-token")
	writeAppTestFile(t, tokenFile, []byte(token))
	return scopedModelFixture{store: store, rootToken: root, workspace: workspace, path: project, tokenFile: tokenFile}
}

func (f scopedModelFixture) arguments() []string {
	return []string{
		"--root", f.store.Root, "models", "--provider", config.ProviderFixture, "--cwd", f.path,
		"--expected-root", f.rootToken, "--expected-workspace-file", f.tokenFile, "--relative-project", "project",
	}
}

func scopedModelCatalogWithPrepare(t *testing.T, path string, prepare commonprovider.PrepareCandidate) commonprovider.Catalog {
	t.Helper()
	catalog := modelsFailureCatalog(t, path, nil)
	registration, err := catalog.Lookup(config.ProviderFixture)
	if err != nil {
		t.Fatal(err)
	}
	registration.Prepare = prepare
	catalog, err = commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func assertNoModelTasks(t *testing.T, store *taskdir.Store) {
	t.Helper()
	ids, err := store.TaskIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("model discovery admitted ordinary tasks: %v %v", ids, err)
	}
}

func TestScopedModelArgumentsRequireCompleteSelection(t *testing.T) {
	valid := []string{"models", "--provider", config.ProviderFixture, "--expected-root", strings.Repeat("a", 64), "--expected-workspace-file", "/workspace-token", "--relative-project", "."}
	if _, err := ParseArguments(valid); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{
		{"--expected-root", strings.Repeat("a", 64)},
		{"--expected-workspace-file", "/workspace-token", "--relative-project", "."},
		{"--expected-root", strings.Repeat("a", 64), "--expected-workspace-file", "relative", "--relative-project", "."},
		{"--expected-project", "legacy-token"},
		{"--origin", "/origin.json"},
	} {
		if _, err := ParseArguments(append([]string{"models", "--provider", config.ProviderFixture}, flags...)); err == nil {
			t.Fatalf("accepted incomplete or unrelated guards: %v", flags)
		}
	}
}

func TestScopedModelsPublicRefusesBeforeProviderPreparationOrStateMutation(t *testing.T) {
	for _, name := range []string{"wrong root", "wrong cwd", "physical approval", "invalid relative path", "empty token", "oversized token", "fifo token", "symlink token", "missing root"} {
		t.Run(name, func(t *testing.T) {
			f := newScopedModelFixture(t, nil)
			args := f.arguments()
			configureScopedModelRefusal(t, name, &f, args)
			before := snapshotRebootTaskFiles(t, f.store.Root)
			preparations := 0
			catalog := scopedModelCatalogWithPrepare(t, f.path, func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
				preparations++
				return commonprovider.ProfileCandidate{}, errors.New("unexpected preparation")
			})
			var stdout, stderr bytes.Buffer
			code := Run(append(args, "--json"), &stdout, &stderr, Dependencies{Catalog: catalog})
			var response ModelsResponse
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("invalid response: code=%d stdout=%s stderr=%s error=%v", code, &stdout, &stderr, err)
			}
			if code == 0 || response.Failure == nil || len(response.Models) != 0 || preparations != 0 || stderr.Len() != 0 {
				t.Fatalf("invalid scope reached provider: preparations=%d code=%d response=%+v stderr=%s", preparations, code, response, &stderr)
			}
			if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, f.store.Root)) {
				t.Fatal("refused scope changed Native state")
			}
			if name == "missing root" {
				if _, err := os.Stat(args[1]); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("guarded discovery initialized a missing root: %v", err)
				}
			}
		})
	}
}

func configureScopedModelRefusal(t *testing.T, name string, f *scopedModelFixture, args []string) {
	t.Helper()
	switch name {
	case "wrong root":
		args[8] = strings.Repeat("f", 64)
	case "wrong cwd":
		args[6] = f.workspace
	case "invalid relative path":
		args[12] = "../escape"
	case "physical approval":
		identity, err := taskdir.ObserveProjectContext(t.Context(), f.workspace)
		if err != nil {
			t.Fatal(err)
		}
		token, err := taskdir.ProjectToken(identity)
		if err != nil {
			t.Fatal(err)
		}
		writeAppTestFile(t, f.tokenFile, []byte(token))
	case "empty token":
		writeAppTestFile(t, f.tokenFile, nil)
	case "oversized token":
		writeAppTestFile(t, f.tokenFile, bytes.Repeat([]byte("a"), task.MaxControlRecordSize+1))
	case "fifo token", "symlink token":
		if err := os.Remove(f.tokenFile); err != nil {
			t.Fatal(err)
		}
		var err error
		if name == "fifo token" {
			err = unix.Mkfifo(f.tokenFile, 0o600)
		} else {
			err = os.Symlink(f.workspace, f.tokenFile)
		}
		if err != nil {
			t.Fatal(err)
		}
	case "missing root":
		args[1] = filepath.Join(t.TempDir(), "absent-native")
	}
}

func TestScopedModelsDerivesBindingBeforeCandidateAndAllowsOmittedCwd(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(map[bool]string{false: "derived cwd", true: "explicit cwd"}[supplied], func(t *testing.T) {
			f := newScopedModelFixture(t, nil)
			a, err := ParseArguments(f.arguments())
			if err != nil {
				t.Fatal(err)
			}
			if !supplied {
				a.Cwd = ""
			}
			var captured task.TaskRecord
			stop := errors.New("stop after guarded request construction")
			catalog := scopedModelCatalogWithPrepare(t, f.path, func(request task.TaskRecord, _ []string) (commonprovider.ProfileCandidate, error) {
				captured = request
				return commonprovider.ProfileCandidate{}, stop
			})
			_, err = discoverModels(a, Dependencies{Catalog: catalog}.normalized())
			if !errors.Is(err, stop) || captured.ConnectorContext == nil {
				t.Fatalf("candidate received no guarded request: request=%+v error=%v", captured, err)
			}
			if captured.Origin != nil || captured.CanonicalCwd != f.path || !captured.ConnectorContext.Project.IsPath() || captured.ConnectorContext.RootToken != f.rootToken {
				t.Fatalf("derived binding changed the selected authority: %+v", captured)
			}
			if err = task.ValidateTaskRecord(&captured); err != nil {
				t.Fatalf("derived model request is invalid: %v", err)
			}
			assertNoModelTasks(t, f.store)
		})
	}
}

func TestScopedModelFeatureIsOnlyAnExecutableProjection(t *testing.T) {
	f := newScopedModelFixture(t, nil)
	before := snapshotRebootTaskFiles(t, f.store.Root)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--root", f.store.Root, "context", "inspect", "--json"}, &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if code != 0 || response.Connector == nil || !slices.Contains(response.Connector.Features, task.ScopedModelDiscoveryFeature) || stderr.Len() != 0 {
		t.Fatalf("context omitted actual scoped model support: code=%d response=%+v stderr=%s", code, response, &stderr)
	}
	if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, f.store.Root)) {
		t.Fatal("executable capability projection changed persistent root state")
	}
}
