package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func writeMarkerGit(t *testing.T, directory, who string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s' " + who + " > \"$FAKE_GIT_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(directory, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// The selected PATH=. opt-out resolves to the bare name "git". The command
// must carry that exact selected Path rather than search the process PATH
// again, and keep the original argv[0].
func TestGitRunExecutesSelectedRelativeLookupNotAmbientPath(t *testing.T) {
	selected, ambient := canonicalWorkspaceTestDir(t), canonicalWorkspaceTestDir(t)
	writeMarkerGit(t, selected, "selected")
	writeMarkerGit(t, ambient, "ambient")
	t.Setenv("PATH", ambient)
	t.Chdir(selected)
	marker := filepath.Join(canonicalWorkspaceTestDir(t), "marker")
	environment := []string{"PATH=.", "GODEBUG=execerrdot=0", "FAKE_GIT_MARKER=" + marker}
	caller := slices.Clone(environment)
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error { return command.Run() }}

	git := Git{Runner: runner, Environment: environment}
	if err := git.run(t.Context(), selected, []string{"status"}, nil, nil, nil, nil); err != nil {
		t.Fatalf("selected relative lookup: %v", err)
	}
	command := runner.calls[0]
	if command.Err != nil || command.Path != "git" || command.Args[0] != "git" {
		t.Fatalf("command = path %q argv0 %q err %v; want selected path and original argv0 \"git\"", command.Path, command.Args[0], command.Err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "selected" {
		t.Fatalf("executed program marker = %q, %v; want selected", got, err)
	}
	if !slices.Equal(environment, caller) {
		t.Fatalf("caller environment changed: %q", environment)
	}
}

func TestGitRunKeepsDefaultErrDotAbsoluteSelectionAndMissingPath(t *testing.T) {
	selected := canonicalWorkspaceTestDir(t)
	writeMarkerGit(t, selected, "selected")
	t.Chdir(selected)
	tests := []struct {
		name        string
		environment []string
		wantPath    string
		wantErr     error
	}{
		{name: "default relative keeps ErrDot", environment: []string{"PATH=."}, wantErr: exec.ErrDot},
		{name: "absolute selection", environment: []string{"PATH=" + selected}, wantPath: filepath.Join(selected, "git")},
		{name: "missing selected PATH", environment: []string{}, wantErr: exec.ErrNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingGitRunner{}
			err := Git{Runner: runner, Environment: test.environment}.run(t.Context(), selected, []string{"status"}, nil, nil, nil, nil)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || len(runner.calls) != 0 {
					t.Fatalf("lookup error = %v with %d runner calls; want %v and none", err, len(runner.calls), test.wantErr)
				}
				return
			}
			if err != nil || runner.calls[0].Path != test.wantPath || runner.calls[0].Args[0] != "git" {
				t.Fatalf("command = %+v, %v; want path %q argv0 git", runner.calls[0], err, test.wantPath)
			}
		})
	}
}
