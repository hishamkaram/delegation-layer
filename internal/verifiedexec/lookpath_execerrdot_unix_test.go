//go:build unix

package verifiedexec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const execErrDotProgram = "selected-program"

// lookupFixture is a relative PATH entry holding one executable, with the
// working directory set to its root so the entry resolves.
func lookupFixture(t *testing.T) (relativeDir string) {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	relativeDir = "bin"
	if err := os.Mkdir(relativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvironmentExecutable(t, filepath.Join(root, relativeDir, execErrDotProgram), 0o700)
	return relativeDir
}

type execErrDotCase struct {
	name       string
	process    string
	selected   []string
	wantOptOut bool
}

func execErrDotCases() []execErrDotCase {
	return []execErrDotCase{
		{name: "selected opt-out", selected: []string{"GODEBUG=execerrdot=0"}, wantOptOut: true},
		{name: "process opt-out alone cannot bypass", process: "execerrdot=0"},
		{name: "process strict with selected opt-out", process: "execerrdot=1", selected: []string{"GODEBUG=execerrdot=0"}, wantOptOut: true},
		{name: "last setting disables", selected: []string{"GODEBUG=execerrdot=1,execerrdot=0"}, wantOptOut: true},
		{name: "last setting restores", selected: []string{"GODEBUG=execerrdot=0,execerrdot=1"}},
		{name: "opt-out among other settings", selected: []string{"GODEBUG=http2client=0,execerrdot=0,panicnil=1"}, wantOptOut: true},
		{name: "first environment entry wins and disables", selected: []string{"GODEBUG=execerrdot=0", "GODEBUG=execerrdot=1"}, wantOptOut: true},
		{name: "first environment entry wins and restores", selected: []string{"GODEBUG=execerrdot=1", "GODEBUG=execerrdot=0"}},
		{name: "absent value", selected: nil},
		{name: "empty value", selected: []string{"GODEBUG="}},
		{name: "empty setting value", selected: []string{"GODEBUG=execerrdot="}},
		{name: "enabled value", selected: []string{"GODEBUG=execerrdot=1"}},
		{name: "non-exact zero", selected: []string{"GODEBUG=execerrdot=00"}},
		{name: "unrelated setting", selected: []string{"GODEBUG=http2client=0"}},
		{name: "unrelated name suffix", selected: []string{"GODEBUG=xexecerrdot=0"}},
	}
}

func TestLookPathExecErrDotOptOutFollowsSelectedGODEBUG(t *testing.T) {
	relativeDir := lookupFixture(t)
	want := filepath.Join(relativeDir, execErrDotProgram)
	for _, test := range execErrDotCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GODEBUG", test.process)
			environment := append([]string{"PATH=" + relativeDir}, test.selected...)
			path, err := LookPath(execErrDotProgram, environment)
			if path != want {
				t.Fatalf("path = %q, want %q (err %v)", path, want, err)
			}
			if test.wantOptOut && err != nil {
				t.Fatalf("selected opt-out returned %v, want nil", err)
			}
			if !test.wantOptOut && !errors.Is(err, exec.ErrDot) {
				t.Fatalf("lookup error = %v, want ErrDot", err)
			}
		})
	}
}

func TestLookPathExecErrDotOptOutKeepsAbsoluteAndMissingResults(t *testing.T) {
	absolute := t.TempDir()
	writeEnvironmentExecutable(t, filepath.Join(absolute, execErrDotProgram), 0o700)
	optOut := "GODEBUG=execerrdot=0"
	path, err := LookPath(execErrDotProgram, []string{"PATH=" + absolute, optOut})
	if err != nil || path != filepath.Join(absolute, execErrDotProgram) {
		t.Fatalf("absolute lookup = %q, %v", path, err)
	}
	path, err = LookPath(execErrDotProgram, []string{"PATH=" + t.TempDir(), optOut})
	if path != "" || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing lookup = %q, %v; want ErrNotFound", path, err)
	}
	path, err = LookPath(execErrDotProgram, []string{optOut})
	if path != "" || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("lookup without PATH = %q, %v; want ErrNotFound", path, err)
	}
}
