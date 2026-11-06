package sourceinventory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPathsSelectsMaintainedLanguage(t *testing.T) {
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
			paths, err := Paths(alias, test.language, false)
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

func TestPathsRejectsEmptySelection(t *testing.T) {
	root := t.TempDir()
	runFormatGit(t, root, "init", "--quiet")
	for _, language := range []string{"go", "python", "javascript", "shell"} {
		paths, err := Paths(root, language, false)
		if len(paths) != 0 || err == nil || !strings.Contains(err.Error(), "no maintained "+language+" sources") {
			t.Fatalf("empty %s selection = %q, %v", language, paths, err)
		}
	}
	paths, err := Paths(root, "go", true)
	if err != nil || len(paths) != 0 {
		t.Fatalf("formatter empty Go selection = %q, %v", paths, err)
	}
}

func TestPathsRejectsInvalidLanguage(t *testing.T) {
	for _, language := range []string{"", "ruby", "Python", ".py"} {
		paths, err := Paths(t.TempDir(), language, false)
		if len(paths) != 0 || err == nil || !strings.Contains(err.Error(), "unsupported source kind") {
			t.Fatalf("invalid language %q = %q, %v", language, paths, err)
		}
	}
}

func TestPathsFailsWithoutGitInventory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	writeFormatFile(t, root, "would-be-fallback.py", []byte("pass\n"), 0o600)
	paths, err := Paths(root, "python", false)
	if len(paths) != 0 || err == nil || !strings.Contains(err.Error(), "inventory maintained paths") {
		t.Fatalf("non-Git language selection = %q, %v", paths, err)
	}
}

func languageSourceFixture(t *testing.T, root, extension string) []string {
	t.Helper()
	names := []string{
		"tracked" + extension, "untracked" + extension,
		"with space" + extension, "with\nnewline" + extension,
		".hidden" + extension,
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
