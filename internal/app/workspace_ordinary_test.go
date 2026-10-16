package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOrdinaryNonGitDispatchIgnoresPlainWorkspaceMarker(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			store, resource := newAppWorkspaceResource(t)
			source := resource.Request().SourceCwd
			cwd := filepath.Join(source, "nested")
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(source, ".delegate-workspace")
			var err error
			if directory {
				err = os.Mkdir(marker, 0o700)
			} else {
				err = os.WriteFile(marker, []byte("ordinary project data"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", canonicalAppTestTempDir(t))
			managed, actualCwd, err := prepareManagedDispatch(Arguments{}, Dependencies{}, store, resource.Request().TaskID, cwd, []byte("ordinary task"))
			if err != nil || managed != nil || actualCwd != cwd {
				t.Fatalf("optional workspace support rejected ordinary non-Git dispatch: cwd=%q err=%v", actualCwd, err)
			}
		})
	}
}

func TestOrdinaryDispatchRejectsOwnedWorkspaceMarker(t *testing.T) {
	source := canonicalAppTestTempDir(t)
	if err := os.Symlink("/fixture/state/workspaces/id", filepath.Join(source, ".delegate-workspace")); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnmanagedWorkspacePath(source); err == nil {
		t.Fatal("managed checkout bypassed owning-root coordination")
	}
}
