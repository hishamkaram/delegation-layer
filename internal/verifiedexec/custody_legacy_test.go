package verifiedexec

// The legacy constructors (NewCommand, NewCommandInDirectory) are used by
// owners that have no lifetime-owned pool. On every supported OS they must bind
// the child to a private copy of the admitted bytes, never to the original path
// or descriptor.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// legacyBinder binds with the private-copy constructor; each owner's views live
// below its own TMPDIR directory, the existing view-location mechanism.
func legacyBinder(t *testing.T, dir string) bindFunc {
	return func(owner string, request CommandRequest) *Command {
		t.Helper()
		views := filepath.Join(dir, "tmp-"+owner)
		if err := os.MkdirAll(views, 0o700); err != nil {
			t.Fatal(err)
		}
		request.Environment = append([]string{"TMPDIR=" + views}, request.Environment...)
		command, err := NewCommandFromRequest(request)
		if err != nil {
			t.Fatalf("bind %s: %v", request.Executable, err)
		}
		t.Cleanup(func() {
			if closeErr := command.Close(); closeErr != nil {
				t.Errorf("close legacy command: %v", closeErr)
			}
		})
		return command
	}
}

func TestLegacyMutationMatrix(t *testing.T)             { runMatrix(t, legacyBinder) }
func TestLegacyNodeEntry(t *testing.T)                  { runNodeEntry(t, legacyBinder) }
func TestLegacyInterpreterSnapshot(t *testing.T)        { runInterpreterCases(t, legacyBinder) }
func TestLegacyViewIsNeverTheOriginalPath(t *testing.T) { requireViewOutsideOriginal(t, legacyBinder) }

// requireViewOutsideOriginal binds a native executable and requires that the
// command path is a private file, not the original and not a /dev/fd path, with
// the original path kept as argv[0] and the caller's arguments after it.
func requireViewOutsideOriginal(t *testing.T, factory binderFactory) {
	dir := scratch(t)
	entry, digest := selfNative(t, filepath.Join(dir, "fixture"))
	command := factory(t, dir)("a", CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg}})
	if command.Cmd.Path == entry || strings.HasPrefix(command.Cmd.Path, "/dev/fd/") {
		t.Fatalf("command path %q is the original or a descriptor path", command.Cmd.Path)
	}
	if command.Cmd.Args[0] != entry || !slices.Equal(command.Cmd.Args[1:], []string{helperArg}) {
		t.Fatalf("argv %q: want the original path then the caller's arguments", command.Cmd.Args)
	}
	originalInfo, err := os.Lstat(entry)
	if err != nil {
		t.Fatal(err)
	}
	viewInfo, err := os.Lstat(command.Cmd.Path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(originalInfo, viewInfo) {
		t.Fatal("the view shares the original inode")
	}
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatal(err)
	}
	if report.Argv0 != entry || report.Marker != digest || !strings.HasPrefix(report.Executable, dir) {
		t.Fatalf("report %+v: want argv0 %s, marker %s, an executable under the scratch tree", report, entry, digest)
	}
}

func TestLegacyConstructorsKeepSignaturesAndArgv(t *testing.T) {
	dir := scratch(t)
	entry, digest := selfNative(t, filepath.Join(dir, "fixture"))
	environment := []string{"TMPDIR=" + dir}
	first, err := NewCommand(entry, digest, environment, helperArg, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCommandInDirectory(entry, digest, dir, environment, helperArg, "two")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		command *Command
		last    string
	}{{first, "one"}, {second, "two"}} {
		if !slices.Equal(c.command.Cmd.Args, []string{entry, helperArg, c.last}) {
			t.Fatalf("argv %q", c.command.Cmd.Args)
		}
		if c.command.Cmd.Dir != "" || c.command.Cmd.Env != nil {
			t.Fatalf("constructor set the working directory or environment: dir %q env %q", c.command.Cmd.Dir, c.command.Cmd.Env)
		}
		if err = c.command.ReleaseDescriptors(); err != nil {
			t.Fatal(err)
		}
		if err = c.command.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyCloseRemovesItsPrivateView(t *testing.T) {
	dir := scratch(t)
	entry, digest := selfNative(t, filepath.Join(dir, "fixture"))
	command, err := NewCommand(entry, digest, []string{"TMPDIR=" + dir}, helperArg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(command.temporaryDir); err != nil {
		t.Fatalf("view missing before Close: %v", err)
	}
	if err = command.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(command.temporaryDir); !os.IsNotExist(err) {
		t.Fatalf("view survived Close: %v", err)
	}
	if err = command.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
