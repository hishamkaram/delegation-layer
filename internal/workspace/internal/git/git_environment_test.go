package git

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitExplicitEnvironmentDoesNotInheritCaller(t *testing.T) {
	const marker = "DELEGATE_GIT_ENVIRONMENT_PROOF"
	t.Setenv(marker, "caller")
	for _, test := range []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "inherited", want: marker + "=caller"},
		{name: "empty", environment: []string{}},
		{name: "selected", environment: []string{marker + "=selected"}, want: marker + "=selected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingGitRunner{}
			git := Git{Runner: runner, Executable: os.Args[0], Environment: test.environment}
			if err := git.run(context.Background(), canonicalWorkspaceTestDir(t), []string{"status"}, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("Git launches = %d, want 1", len(runner.calls))
			}
			assertMarkerEntries(t, runner.calls[0].Env, test.want)
			if os.Getenv(marker) != "caller" {
				t.Fatal("Git changed the caller environment")
			}
		})
	}
}

// assertMarkerEntries requires exactly the wanted proof entry, once, and no
// other value of the proof variable.
func assertMarkerEntries(t *testing.T, environment []string, want string) {
	t.Helper()
	for _, value := range []string{"DELEGATE_GIT_ENVIRONMENT_PROOF=caller", "DELEGATE_GIT_ENVIRONMENT_PROOF=selected"} {
		wantCount := 0
		if value == want {
			wantCount = 1
		}
		if got := countEnvironmentEntry(environment, value); got != wantCount {
			t.Errorf("selected marker %q count = %d, want %d", value, got, wantCount)
		}
	}
}

func TestGitSelectedEnvironmentOwnsRedirectionValidation(t *testing.T) {
	t.Setenv("GIT_DIR", canonicalWorkspaceTestDir(t))
	if err := (Git{Environment: []string{}}).ValidateEnvironment(); err != nil {
		t.Fatalf("caller redirection leaked into the selected environment: %v", err)
	}
	if err := (Git{Environment: []string{"GIT_DIR=selected"}}).ValidateEnvironment(); !errors.Is(err, ErrInvalidGitContext) {
		t.Fatalf("selected redirection was accepted: %v", err)
	}
}

func TestGitSelectedEnvironmentOwnsRefBackendValidation(t *testing.T) {
	t.Setenv("GIT_REFERENCE_BACKEND", "files")
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
		_, err := io.WriteString(command.Stdout, "files\n")
		return err
	}}
	git := Git{Runner: runner, Executable: os.Args[0], Environment: []string{}}
	if err := git.verifyLegacyFilesRefsAbsent(t.Context(), canonicalWorkspaceTestDir(t), []string{absenceTestRef}); err != nil {
		t.Fatalf("caller ref backend override leaked into selected environment: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("selected ref backend observations = %d, want 1", len(runner.calls))
	}
	git.Environment = []string{"GIT_REF_STORAGE_FORMAT=files"}
	if err := git.verifyLegacyFilesRefsAbsent(t.Context(), canonicalWorkspaceTestDir(t), []string{absenceTestRef}); !errors.Is(err, errLegacyRefStorageUnprovable) {
		t.Fatalf("selected ref backend override was accepted: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatal("selected ref backend override reached Git execution")
	}
}

func TestGitSelectedPathOwnsExecutableResolution(t *testing.T) {
	caller, selected := canonicalWorkspaceTestDir(t), canonicalWorkspaceTestDir(t)
	for _, directory := range []string{caller, selected} {
		if err := os.WriteFile(filepath.Join(directory, "git"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", caller)
	runner := &recordingGitRunner{}
	git := Git{Runner: runner, Environment: []string{"PATH=" + selected}}
	if err := git.run(t.Context(), selected, []string{"status"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].Path != filepath.Join(selected, "git") {
		t.Fatal("Git executable did not come from the selected PATH")
	}
}

func TestGitCommandEnvironmentDoesNotAliasSelectedSnapshot(t *testing.T) {
	selected := []string{"DELEGATE_GIT_ENVIRONMENT_PROOF=selected"}
	runner := &recordingGitRunner{}
	git := Git{Runner: runner, Executable: os.Args[0], Environment: selected}
	if err := git.run(t.Context(), canonicalWorkspaceTestDir(t), []string{"status"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for index := range runner.calls[0].Env {
		runner.calls[0].Env[index] = "DELEGATE_GIT_ENVIRONMENT_PROOF=mutated"
	}
	if selected[0] != "DELEGATE_GIT_ENVIRONMENT_PROOF=selected" {
		t.Fatal("Git command environment aliases the selected snapshot")
	}
}
