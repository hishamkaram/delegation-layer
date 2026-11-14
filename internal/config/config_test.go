package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestParseBudget(t *testing.T) {
	// Default
	d, err := ParseBudget("")
	if err != nil || d != DefaultBudgetDuration {
		t.Errorf("expected default budget 30m, got %v, err=%v", d, err)
	}

	// Valid custom
	d, err = ParseBudget("45m")
	if err != nil || d != 45*time.Minute {
		t.Errorf("expected 45m, got %v, err=%v", d, err)
	}

	// Invalid zero/negative
	if _, err := ParseBudget("0s"); !errors.Is(err, ErrInvalidBudget) {
		t.Errorf("expected ErrInvalidBudget for 0s, got: %v", err)
	}
	if _, err := ParseBudget("-5m"); !errors.Is(err, ErrInvalidBudget) {
		t.Errorf("expected ErrInvalidBudget for -5m, got: %v", err)
	}
	if _, err := ParseBudget("invalid"); !errors.Is(err, ErrInvalidBudget) {
		t.Errorf("expected ErrInvalidBudget for invalid string, got: %v", err)
	}
}

func TestParseBudgetForProvider(t *testing.T) {
	if got, err := ParseBudgetForProvider(ProviderCodexReview, ""); err != nil || got != DefaultCodexReviewBudgetDuration {
		t.Fatalf("codex review default=%v, err=%v", got, err)
	}
	if got, err := ParseBudgetForProvider(ProviderCodexExec, ""); err != nil || got != DefaultBudgetDuration {
		t.Fatalf("ordinary default=%v, err=%v", got, err)
	}
	if got, err := ParseBudgetForProvider(ProviderCodexReview, "5m"); err != nil || got != 5*time.Minute {
		t.Fatalf("explicit codex review budget=%v, err=%v", got, err)
	}
}

func TestValidateMode(t *testing.T) {
	if err := ValidateMode(""); err != nil {
		t.Errorf("expected empty mode to default without error, got: %v", err)
	}
	if err := ValidateMode(ModeReadOnly); err != nil {
		t.Errorf("expected read-only to be valid, got: %v", err)
	}
	if err := ValidateMode(ModeWorkspaceWrite); err != nil {
		t.Errorf("expected workspace-write to be valid, got: %v", err)
	}
	if err := ValidateMode("unrestricted"); !errors.Is(err, ErrInvalidMode) {
		t.Errorf("expected ErrInvalidMode, got: %v", err)
	}
}

func TestValidateProvider(t *testing.T) {
	valid := []string{
		ProviderAntigravityPrint,
		ProviderCodexExec,
		ProviderCodexReview,
		ProviderClaudePrint,
		ProviderPiJSON,
		ProviderOpenCodeRun,
		ProviderFixture,
	}
	for _, p := range valid {
		if err := ValidateProvider(p); err != nil {
			t.Errorf("expected provider %s to be valid, got: %v", p, err)
		}
	}
	if err := ValidateProvider("openai:gpt4"); err != nil {
		t.Errorf("expected structurally valid future provider identifier, got: %v", err)
	}
	for _, invalid := range []string{"openai", "openai:", ":exec", "OpenAI:exec", "openai:exec:extra", "openai:exec value"} {
		if err := ValidateProvider(invalid); !errors.Is(err, ErrUnsupportedProvider) {
			t.Errorf("expected ErrUnsupportedProvider for %q, got: %v", invalid, err)
		}
	}
}

func testValidDisjointAndNonexistent(t *testing.T, root, workspace, tmpDir string) {
	cRoot, cCwd, err := ValidateDirectories(root, workspace)
	if err != nil {
		t.Fatalf("expected valid directories, got: %v", err)
	}
	if cRoot == "" || cCwd == "" {
		t.Errorf("expected non-empty canonical paths")
	}

	siblingDir := filepath.Join(tmpDir, "sibling-work")
	if err := os.Mkdir(siblingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidateDirectories(root, siblingDir); err != nil {
		t.Errorf("expected sibling directories not to overlap, got: %v", err)
	}

	if _, _, err := ValidateDirectories(root, filepath.Join(tmpDir, "nonexistent")); !errors.Is(err, ErrInvalidCwd) {
		t.Errorf("expected ErrInvalidCwd for nonexistent cwd, got: %v", err)
	}
}

func testOverlappingDirectories(t *testing.T, root, workspace string) {
	if _, _, err := ValidateDirectories(workspace, workspace); !errors.Is(err, ErrRootWorkspaceOverlap) {
		t.Errorf("expected ErrRootWorkspaceOverlap for identical root and cwd, got: %v", err)
	}
	nestedRoot := filepath.Join(workspace, "nested-root")
	if _, _, err := ValidateDirectories(nestedRoot, workspace); !errors.Is(err, ErrRootWorkspaceOverlap) {
		t.Errorf("expected ErrRootWorkspaceOverlap for root inside cwd, got: %v", err)
	}
	nestedCwd := filepath.Join(root, "nested-workspace")
	if err := os.Mkdir(nestedCwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidateDirectories(root, nestedCwd); !errors.Is(err, ErrRootWorkspaceOverlap) {
		t.Errorf("expected ErrRootWorkspaceOverlap for cwd inside root, got: %v", err)
	}
	dotdotRoot := filepath.Join(workspace, "..state")
	if _, _, err := ValidateDirectories(dotdotRoot, workspace); !errors.Is(err, ErrRootWorkspaceOverlap) {
		t.Errorf("expected ErrRootWorkspaceOverlap for ..state inside workspace, got: %v", err)
	}
	dotdotCwd := filepath.Join(root, "..workspace")
	if err := os.Mkdir(dotdotCwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidateDirectories(root, dotdotCwd); !errors.Is(err, ErrRootWorkspaceOverlap) {
		t.Errorf("expected ErrRootWorkspaceOverlap for ..workspace inside root, got: %v", err)
	}
}

func TestValidateDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmpDir, "state-root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	testValidDisjointAndNonexistent(t, root, workspace, tmpDir)
	testOverlappingDirectories(t, root, workspace)
}

func TestValidateBriefFile(t *testing.T) {
	tmpDir := t.TempDir()
	validBrief := filepath.Join(tmpDir, "brief.md")
	content := []byte("Hello world brief content")
	if err := os.WriteFile(validBrief, content, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Valid file
	size, err := ValidateBriefFile(validBrief)
	if err != nil || size != int64(len(content)) {
		t.Errorf("expected valid brief of size %d, got %d, err=%v", len(content), size, err)
	}

	// 2. Nonexistent file
	if _, err := ValidateBriefFile(filepath.Join(tmpDir, "missing.md")); !errors.Is(err, ErrBriefNotFound) {
		t.Errorf("expected ErrBriefNotFound, got: %v", err)
	}

	// 3. Directory
	dirPath := filepath.Join(tmpDir, "dir")
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBriefFile(dirPath); !errors.Is(err, ErrBriefNotRegular) {
		t.Errorf("expected ErrBriefNotRegular for directory, got: %v", err)
	}

	// 4. Symlink
	symPath := filepath.Join(tmpDir, "symlink.md")
	if err := os.Symlink(validBrief, symPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBriefFile(symPath); !errors.Is(err, ErrBriefNotRegular) {
		t.Errorf("expected ErrBriefNotRegular for symlink, got: %v", err)
	}
}

func TestCaseAliasOverlapUsesFilesystemIdentity(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "WORKSPACE")
	actual, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	aliased, err := os.Stat(alias)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(alias, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if _, _, validateErr := ValidateDirectories(alias, workspace); validateErr != nil {
			t.Fatalf("distinct case-sensitive siblings rejected: %v", validateErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(actual, aliased) {
		t.Fatal("case alias unexpectedly resolved to another inode")
	}
	checkCaseAliasOverlap(t, base, workspace, alias)
}

func checkCaseAliasOverlap(t *testing.T, base, workspace, alias string) {
	t.Helper()
	canonical, err := CanonicalizePath(alias)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CanonicalizePath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != want {
		t.Fatalf("same inode not canonicalized: %s versus %s", canonical, want)
	}
	nested := filepath.Join(workspace, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{alias, workspace}, {filepath.Join(alias, "state"), workspace}, {alias, nested}} {
		if _, _, err := ValidateDirectories(pair[0], pair[1]); !errors.Is(err, ErrRootWorkspaceOverlap) {
			t.Fatalf("same-inode overlap accepted %v: %v", pair, err)
		}
	}
	sibling := filepath.Join(base, "sibling")
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ValidateDirectories(sibling, alias); err != nil {
		t.Fatalf("disjoint sibling rejected: %v", err)
	}
}

func TestCanonicalAliasPreservesLexicalSelection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		missingName string
		wantMissing bool
	}{
		{name: "first matching alias"},
		{name: "earlier metadata error", missingName: "a-missing", wantMissing: true},
		{name: "later metadata error", missingName: "zz-missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, entries, info := canonicalAliasSnapshot(t, tc.missingName)
			got, err := canonicalAlias(base, entries, info)
			if tc.wantMissing {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("alias lookup error = %v, want missing metadata", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(base, "b-first"); got != want {
				t.Fatalf("canonical alias = %q, want lexical first %q", got, want)
			}
		})
	}
}

func canonicalAliasSnapshot(t *testing.T, missingName string) (string, []os.DirEntry, os.FileInfo) {
	t.Helper()
	base := t.TempDir()
	target := filepath.Join(base, "z-last")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(base, "b-first")); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, missingName)
	if missingName != "" {
		if err := os.WriteFile(missing, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if missingName != "" {
		// DirEntry may cache metadata, so deletion cannot force Info to fail.
		for index, entry := range entries {
			if entry.Name() == missingName {
				entries[index] = metadataErrorDirEntry{
					DirEntry: entry,
					err:      &os.PathError{Op: "lstat", Path: missing, Err: os.ErrNotExist},
				}
			}
		}
	}
	// Reverse the sorted snapshot to require the same lexical winner and
	// metadata error precedence from unordered native directory entries.
	slices.Reverse(entries)
	return base, entries, info
}

type metadataErrorDirEntry struct {
	os.DirEntry
	err error
}

func (entry metadataErrorDirEntry) Info() (os.FileInfo, error) {
	return nil, entry.err
}

func TestCanonicalDirectoryReadRejectsNonDirectories(t *testing.T) {
	base := t.TempDir()
	regular := filepath.Join(base, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(base, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep a nonblocking writer open so a missing O_DIRECTORY cannot hang the
	// test. Requiring the open error still detects that flags regression.
	fifoGuard, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fifoGuard.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, path := range []string{regular, fifo} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			entries, err := readCanonicalDirectoryEntries(path)
			var pathErr *os.PathError
			if !errors.Is(err, syscall.ENOTDIR) || !errors.As(err, &pathErr) || pathErr.Op != "open" {
				t.Fatalf("directory read error = %v, want open ENOTDIR", err)
			}
			if entries != nil {
				t.Fatalf("failed directory read returned partial entries: %v", entries)
			}
		})
	}
}
