package sourceinventory

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPathsRejectsInvalidRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "source.go")
	writeFormatFile(t, root, "source.go", []byte("package fixture\n"), 0o600)
	for _, invalid := range []string{"", file, filepath.Join(root, "missing")} {
		t.Run(invalid, func(t *testing.T) {
			paths, err := Paths(invalid, "go", true)
			if err == nil || len(paths) != 0 {
				t.Fatalf("invalid root returned paths=%q error=%v", paths, err)
			}
		})
	}
}

func TestPathsRejectsEmptyAllowanceForLinterKinds(t *testing.T) {
	for _, kind := range []string{"python", "javascript", "shell"} {
		paths, err := Paths(t.TempDir(), kind, true)
		if len(paths) != 0 || err == nil {
			t.Fatalf("empty allowance for %s returned %q, %v", kind, paths, err)
		}
	}
}

func TestPathsRejectsMalformedGitRecords(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("source.go"), []byte("\x00"), []byte("source.go\x00\x00"),
		[]byte("../outside.go\x00"), []byte("/outside.go\x00"),
		[]byte("./source.go\x00"), []byte(".\x00"),
	} {
		t.Run(string(data), func(t *testing.T) {
			root := fakeGitInventory(t, data, 0)
			paths, err := Paths(root, "go", true)
			if err == nil || len(paths) != 0 {
				t.Fatalf("malformed inventory returned paths=%q error=%v", paths, err)
			}
		})
	}
}

func TestPathsSortsAndDeduplicatesGitRecords(t *testing.T) {
	root := fakeGitInventory(t, []byte("b.py\x00a.py\x00a.py\x00"), 0)
	for _, name := range []string{"a.py", "b.py"} {
		writeFormatFile(t, root, name, []byte("pass\n"), 0o600)
	}
	paths, err := Paths(root, "python", false)
	want := []string{filepath.Join(root, "a.py"), filepath.Join(root, "b.py")}
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("deduplicated paths=%q, want=%q, error=%v", paths, want, err)
	}
}

func TestPathsPreservesGitFailure(t *testing.T) {
	root := fakeGitInventory(t, nil, 42)
	paths, err := Paths(root, "go", true)
	var exitError *exec.ExitError
	if len(paths) != 0 || !errors.As(err, &exitError) || exitError.ExitCode() != 42 {
		t.Fatalf("Git failure paths=%q error=%v", paths, err)
	}
}

func TestPathsRejectsRepositorySubdirectory(t *testing.T) {
	root := t.TempDir()
	runFormatGit(t, root, "init", "--quiet")
	writeFormatFile(t, root, ".gitignore", []byte(".local/\n"), 0o600)
	writeFormatFile(t, root, "nested/source.go", []byte("package fixture\n"), 0o600)
	writeFormatFile(t, root, ".local/source.go", []byte("package ignored\n"), 0o600)
	for _, directory := range []string{"nested", ".local"} {
		t.Run(directory, func(t *testing.T) {
			paths, err := Paths(filepath.Join(root, directory), "go", true)
			if len(paths) != 0 || err == nil || !strings.Contains(err.Error(), "source inventory root must be the repository root") {
				t.Fatalf("repository subdirectory returned paths=%q error=%v", paths, err)
			}
		})
	}
}

func TestPathsPreservesGitFailureDiagnostics(t *testing.T) {
	for _, operation := range []string{"rev-parse", "ls-files"} {
		t.Run(operation, func(t *testing.T) {
			root := fakeGitInventory(t, nil, 42)
			t.Setenv("SOURCE_INVENTORY_TEST_FAIL_COMMAND", operation)
			t.Setenv("SOURCE_INVENTORY_TEST_STDERR", "  controlled Git diagnostic\n\n")
			paths, err := Paths(root, "go", true)
			var exitError *exec.ExitError
			if len(paths) != 0 || !errors.As(err, &exitError) || exitError.ExitCode() != 42 {
				t.Fatalf("Git failure paths=%q error=%v", paths, err)
			}
			if !strings.Contains(err.Error(), "inventory maintained paths") || !strings.HasSuffix(err.Error(), ": controlled Git diagnostic") {
				t.Fatalf("Git failure lost context or diagnostic: %v", err)
			}
		})
	}
}

func TestPathsRejectsSourceSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	runFormatGit(t, root, "init", "--quiet")
	outside := t.TempDir()
	writeFormatFile(t, outside, "outside.go", []byte("package outside\n"), 0o600)
	if err := os.Symlink(filepath.Join(outside, "outside.go"), filepath.Join(root, "linked.go")); err != nil {
		t.Fatal(err)
	}
	paths, err := Paths(root, "go", true)
	if len(paths) != 0 || err == nil {
		t.Fatalf("source symlink returned paths=%q error=%v", paths, err)
	}
	if removeErr := os.Remove(filepath.Join(root, "linked.go")); removeErr != nil {
		t.Fatal(removeErr)
	}
	writeFormatFile(t, root, "package/outside.go", []byte("package fixture\n"), 0o600)
	runFormatGit(t, root, "add", "package/outside.go")
	if removeErr := os.RemoveAll(filepath.Join(root, "package")); removeErr != nil {
		t.Fatal(removeErr)
	}
	if linkErr := os.Symlink(outside, filepath.Join(root, "package")); linkErr != nil {
		t.Fatal(linkErr)
	}
	paths, err = Paths(root, "go", true)
	if len(paths) != 0 || err == nil {
		t.Fatalf("parent symlink returned paths=%q error=%v", paths, err)
	}
}

func fakeGitInventory(t *testing.T, data []byte, exitCode int) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFormatFile(t, root, "inventory.nul", data, 0o600)
	writeFormatFile(t, root, "bin/git", []byte(`#!/bin/sh
if [ "$1" = rev-parse ] && [ "$SOURCE_INVENTORY_TEST_FAIL_COMMAND" != rev-parse ]; then
    printf '\n'
    exit 0
fi
cat "$SOURCE_INVENTORY_TEST_OUTPUT"
printf '%s' "$SOURCE_INVENTORY_TEST_STDERR" >&2
exit "$SOURCE_INVENTORY_TEST_EXIT"
`), 0o700)
	t.Setenv("PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOURCE_INVENTORY_TEST_OUTPUT", filepath.Join(root, "inventory.nul"))
	t.Setenv("SOURCE_INVENTORY_TEST_EXIT", fmt.Sprint(exitCode))
	t.Setenv("SOURCE_INVENTORY_TEST_FAIL_COMMAND", "ls-files")
	t.Setenv("SOURCE_INVENTORY_TEST_STDERR", "")
	return root
}
