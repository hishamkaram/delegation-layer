package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceSelectionCLIEmitsNULSeparatedPaths(t *testing.T) {
	root := sourceRepository(t)
	names := []string{"a source.py", "another\nsource.py"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte("pass\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code, runErr := run([]string{"--root", root, "--sources", "python"}, &stdout, &stderr)
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonical, names[0]) + "\x00" + filepath.Join(canonical, names[1]) + "\x00"
	if code != 0 || runErr != nil || stderr.Len() != 0 || stdout.String() != want {
		t.Fatalf("source selection: exit=%d stderr=%q stdout=%q want=%q", code, stderr.String(), stdout.String(), want)
	}
}

func TestSourceSelectionCLIRejectsConflictingOptions(t *testing.T) {
	cases := [][]string{
		{"--sources", "go", "--go-sources"},
		{"--sources", "python", "--strict"},
		{"--sources", "shell", "--json"},
		{"--go-sources", "--strict"},
		{"--go-sources", "--json"},
		{"--sources", "go", "unexpected"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code, runErr := run(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || runErr == nil {
			t.Fatalf("args=%q: exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestSourceSelectionCLIFailsForEmptyOrInvalidSelection(t *testing.T) {
	root := sourceRepository(t)
	for _, language := range []string{"python", "", "unknown"} {
		var stdout, stderr bytes.Buffer
		args := []string{"--root", root, "--sources", language}
		if code, runErr := run(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || runErr == nil {
			t.Fatalf("language=%q: exit=%d stdout=%q stderr=%q", language, code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code, runErr := run([]string{"--root", root, "--go-sources"}, &stdout, &stderr); code != 0 || runErr != nil || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("legacy empty selection: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSourceSelectionCLIReportsOutputFailure(t *testing.T) {
	root := sourceRepository(t)
	if err := os.WriteFile(filepath.Join(root, "source.py"), []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code, runErr := run([]string{"--root", root, "--sources", "python"}, failedSourceWriter{}, &stderr)
	if code != 2 || runErr == nil || !strings.Contains(runErr.Error(), "source output failed") {
		t.Fatalf("output failure: exit=%d stderr=%q", code, stderr.String())
	}
}

func TestSourceSelectionCLIHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, runErr := run([]string{"--help"}, &stdout, &stderr)
	if code != 0 || runErr != nil || !strings.Contains(stderr.String(), "-sources") {
		t.Fatalf("help: exit=%d stderr=%q", code, stderr.String())
	}
}

type failedSourceWriter struct{}

func (failedSourceWriter) Write(_ []byte) (int, error) { return 0, errors.New("source output failed") }

func sourceRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	command := exec.Command("git", "init", "--quiet", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("initialize source repository: %v: %s", err, output)
	}
	return root
}
