package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/provider/contract"
)

type placementTree struct {
	base  string
	state string
	work  string
	cache string
	alias string
}

func newPlacementTree(t *testing.T) placementTree {
	t.Helper()
	base, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatalf("CanonicalizePath() error = %v", err)
	}
	tree := placementTree{
		base:  base,
		state: filepath.Join(base, "state"),
		work:  filepath.Join(base, "work"),
		cache: filepath.Join(base, "cache"),
		alias: filepath.Join(base, "alias"),
	}
	for _, dir := range []string{tree.state, tree.work, tree.cache} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}
	if err = os.Symlink(tree.work, tree.alias); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	return tree
}

func wantRoot(path string) string {
	return fmt.Sprintf("%v: provider writable root is not canonical: %s", contract.ErrProfileUnavailable, path)
}

func wantOverlap(path string) string {
	return fmt.Sprintf("%v: state root overlaps provider-writable tree %s", contract.ErrProfileUnavailable, path)
}

func TestObservePathReportsCanonicalSpellingAndFailure(t *testing.T) {
	tree := newPlacementTree(t)
	aliasFact := ObservePath(tree.alias)
	if aliasFact.Path != tree.alias || aliasFact.Canonical != tree.work || aliasFact.Err != nil {
		t.Fatalf("ObservePath(alias) = %+v, want canonical %q", aliasFact, tree.work)
	}
	missing := filepath.Join(tree.base, "not-created", "child")
	missingFact := ObservePath(missing)
	if missingFact.Err != nil || missingFact.Canonical != missing {
		t.Fatalf("ObservePath(missing) = %+v, want its own spelling", missingFact)
	}
	relative := ObservePath("relative/path")
	if relative.Err == nil {
		t.Fatalf("ObservePath(relative) = %+v, want an observation error", relative)
	}
}

func TestValidateStatePlacementAcceptsCanonicalDisjointTrees(t *testing.T) {
	tree := newPlacementTree(t)
	if err := ValidateStatePlacement(tree.state, []string{tree.work, tree.cache}); err != nil {
		t.Fatalf("ValidateStatePlacement() error = %v, want nil", err)
	}
	if err := ValidateStatePlacement(tree.state, nil); err != nil {
		t.Fatalf("ValidateStatePlacement(no roots) error = %v, want nil", err)
	}
}

func TestValidateStatePlacementRejectsAliasedRoots(t *testing.T) {
	tree := newPlacementTree(t)
	stateAlias := filepath.Join(tree.base, "state-alias")
	if err := os.Symlink(tree.state, stateAlias); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	err := ValidateStatePlacement(stateAlias, []string{tree.work})
	if want := contract.ErrProfileUnavailable.Error() + ": state root is not canonical"; err == nil || err.Error() != want {
		t.Fatalf("aliased state root error = %v, want %q", err, want)
	}
	err = ValidateStatePlacement(tree.state, []string{tree.alias})
	if !errors.Is(err, contract.ErrProfileUnavailable) || err.Error() != wantRoot(tree.alias) {
		t.Fatalf("aliased writable root error = %v, want %q", err, wantRoot(tree.alias))
	}
}

func TestValidateStatePlacementRejectsOverlapThroughNestedAndParentRoots(t *testing.T) {
	tree := newPlacementTree(t)
	nested := filepath.Join(tree.state, "cache")
	for _, writable := range []string{tree.state, nested, tree.base} {
		err := ValidateStatePlacement(tree.state, []string{writable})
		if !errors.Is(err, contract.ErrProfileUnavailable) || err.Error() != wantOverlap(writable) {
			t.Errorf("writable %q error = %v, want %q", writable, err, wantOverlap(writable))
		}
	}
}

func TestValidateStatePlacementJudgesRootBeforeAnyWritableRoot(t *testing.T) {
	tree := newPlacementTree(t)
	err := ValidateStatePlacement("relative/state", []string{tree.state, "also/relative"})
	if want := contract.ErrProfileUnavailable.Error() + ": state root is not canonical"; err == nil || err.Error() != want {
		t.Fatalf("error = %v, want state root failure %q", err, want)
	}
}

func TestValidateStatePlacementReportsFirstFailingRootInOrder(t *testing.T) {
	tree := newPlacementTree(t)
	overlap := filepath.Join(tree.state, "cache")
	cases := []struct {
		name  string
		roots []string
		want  string
	}{
		{"overlap before non-canonical", []string{tree.work, overlap, "relative"}, wantOverlap(overlap)},
		{"non-canonical before overlap", []string{tree.work, tree.alias, overlap}, wantRoot(tree.alias)},
		{"relative before overlap", []string{"relative", overlap}, wantRoot("relative")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStatePlacement(tree.state, tc.roots)
			if !errors.Is(err, contract.ErrProfileUnavailable) || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
