package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
)

func TestProfileStatePlacementExcludesRuntimeRoots(t *testing.T) {
	base, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace")
	cache := filepath.Join(base, "cache")
	for _, path := range []string{workspace, cache} {
		if err = os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	profile := PreparedProfile{Plan: execution.Plan{Directory: workspace}, WritableRoots: []string{cache}}
	for _, root := range []string{cache, filepath.Join(cache, "state"), base, workspace, filepath.Join(workspace, "state")} {
		if err = profile.ValidateStatePlacement(root); !errors.Is(err, ErrProfileUnavailable) {
			t.Errorf("overlapping state root %s accepted: %v", root, err)
		}
	}
	for _, root := range []string{filepath.Join(base, "state"), filepath.Join(base, "cache-sibling")} {
		if err = profile.ValidateStatePlacement(root); err != nil {
			t.Errorf("disjoint state root %s rejected: %v", root, err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err = os.Symlink(cache, alias); err != nil {
		t.Fatal(err)
	}
	if err = profile.ValidateStatePlacement(filepath.Join(alias, "state")); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("aliased root accepted: %v", err)
	}
	profile.WritableRoots = []string{alias}
	if err = profile.ValidateStatePlacement(filepath.Join(base, "state")); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("aliased writable root accepted: %v", err)
	}
}

func TestProfileStatePlacementRechecksFilesystemAliases(t *testing.T) {
	base, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, cache := filepath.Join(base, "state"), filepath.Join(base, "future-cache")
	profile := PreparedProfile{Plan: execution.Plan{Directory: filepath.Join(base, "workspace")}, WritableRoots: []string{cache}}
	if err = profile.ValidateStatePlacement(root); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(root, cache); err != nil {
		t.Fatal(err)
	}
	if err = profile.ValidateStatePlacement(root); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("new alias into state accepted: %v", err)
	}
}
