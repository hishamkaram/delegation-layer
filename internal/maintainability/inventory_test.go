package maintainability

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestDiagramAuthoringSourcesKeepDocumentationBudget(t *testing.T) {
	cases := []struct {
		path string
		kind string
		max  int
	}{
		{path: "docs/architecture.md", kind: "documentation", max: 1800},
		{path: "diagrams/components.architecture.json", kind: "documentation", max: 1800},
		{path: "diagrams/renamed.sequence.json", kind: "documentation", max: 1800},
		{path: "diagrams/config.json", kind: "code", max: 800},
		{path: "internal/layout.architecture.json", kind: "code", max: 800},
	}
	manifest := policy.Manifest{Limits: policy.Limits{Code: 800, Tests: 1000, Documentation: 1800, Function: 80}}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			kind := sourceKind(test.path)
			if kind != test.kind {
				t.Fatalf("source kind = %q, want %q", kind, test.kind)
			}
			source := policy.Source{Path: test.path, Kind: kind, Lines: test.max}
			if findings := policy.Check(manifest, []policy.Source{source}); len(findings) != 0 {
				t.Fatalf("exact budget rejected: %+v", findings)
			}
			source.Lines++
			findings := policy.Check(manifest, []policy.Source{source})
			if len(findings) != 1 || findings[0].Kind != "size" || findings[0].Limit != test.max {
				t.Fatalf("over-budget source not rejected: %+v", findings)
			}
		})
	}
}

func TestSourcePathsSelectsMaintainedLanguage(t *testing.T) {
	root := t.TempDir()
	runFormatGit(t, root, "init", "--quiet")
	runFormatGit(t, root, "config", "core.excludesFile", os.DevNull)
	writeFormatFile(t, root, ".gitignore", []byte(".local/\n"), 0o600)
	cases := []struct{ language, extension string }{
		{"go", ".go"}, {"python", ".py"}, {"javascript", ".mjs"}, {"shell", ".sh"},
	}
	for _, test := range cases {
		t.Run(test.language, func(t *testing.T) {
			names := languageSourceFixture(t, root, test.extension)
			alias := filepath.Join(t.TempDir(), "linked repository")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			paths, err := SourcePaths(alias, test.language)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			want := make([]string, 0, len(names))
			for _, name := range names {
				want = append(want, filepath.Join(canonical, name))
			}
			slices.Sort(want)
			if !slices.Equal(paths, want) {
				t.Fatalf("paths = %q, want %q", paths, want)
			}
		})
	}
}

func TestSourcePathsRejectsEmptySelection(t *testing.T) {
	root := t.TempDir()
	runFormatGit(t, root, "init", "--quiet")
	for _, language := range []string{"go", "python", "javascript", "shell"} {
		paths, err := SourcePaths(root, language)
		if len(paths) != 0 || err == nil || !strings.Contains(err.Error(), "no maintained "+language+" sources") {
			t.Fatalf("empty %s selection = %q, %v", language, paths, err)
		}
	}
	paths, err := GoSourcePaths(root)
	if err != nil || len(paths) != 0 {
		t.Fatalf("legacy empty Go selection = %q, %v", paths, err)
	}
}

func TestSourcePathsRejectsInvalidLanguage(t *testing.T) {
	for _, language := range []string{"", "ruby", "Python", ".py"} {
		paths, err := SourcePaths(t.TempDir(), language)
		if len(paths) != 0 || err == nil || !strings.Contains(err.Error(), "unsupported source language") {
			t.Fatalf("invalid language %q = %q, %v", language, paths, err)
		}
	}
}

func TestSourcePathsFailsWithoutGitInventory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", root)
	writeFormatFile(t, root, "would-be-fallback.py", []byte("pass\n"), 0o600)
	paths, err := SourcePaths(root, "python")
	if len(paths) != 0 || err == nil || !strings.Contains(err.Error(), "inventory maintained paths") {
		t.Fatalf("non-Git language selection = %q, %v", paths, err)
	}
}

func languageSourceFixture(t *testing.T, root, extension string) []string {
	t.Helper()
	names := []string{
		"tracked" + extension, "untracked" + extension,
		"with space" + extension, "with\nnewline" + extension,
	}
	for _, name := range names {
		writeFormatFile(t, root, name, []byte("source\n"), 0o600)
	}
	writeFormatFile(t, root, "deleted"+extension, []byte("source\n"), 0o600)
	writeFormatFile(t, root, ".local/ignored"+extension, []byte("source\n"), 0o600)
	writeFormatFile(t, root, "not-source"+extension+".snapshot", []byte("source\n"), 0o600)
	runFormatGit(t, root, "add", "--", names[0], "deleted"+extension)
	if err := os.Remove(filepath.Join(root, "deleted"+extension)); err != nil {
		t.Fatal(err)
	}
	return names
}
