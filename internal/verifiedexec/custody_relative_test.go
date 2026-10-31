package verifiedexec

// A relative TMPDIR in the launch environment is interpreted by the child
// against its own working directory. The view must therefore be created where
// the child would look, and be addressed absolutely, so Command.Dir differing
// from this process's directory cannot change which file is launched.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// relativeLaunch is a parent directory holding the process's cwd, a child
// directory used as the launch directory, and a relative TMPDIR present in
// both so that either interpretation can succeed at construction.
type relativeLaunch struct {
	base, childDir string
	environment    []string
}

func newRelativeLaunch(t *testing.T) relativeLaunch {
	t.Helper()
	base := scratch(t)
	childDir := filepath.Join(base, "child")
	for _, dir := range []string{filepath.Join(base, "tmp-rel"), childDir, filepath.Join(childDir, "tmp-rel")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(base)
	return relativeLaunch{base: base, childDir: childDir, environment: []string{"TMPDIR=tmp-rel"}}
}

func (l relativeLaunch) requireViewInChildTmp(t *testing.T, command *Command) {
	t.Helper()
	if !filepath.IsAbs(command.temporaryDir) {
		t.Fatalf("the view %q is relative; it must be addressed absolutely", command.temporaryDir)
	}
	want := filepath.Join(l.childDir, "tmp-rel")
	if filepath.Dir(command.temporaryDir) != want {
		t.Fatalf("the view is below %q; a relative TMPDIR of the child belongs below %q", filepath.Dir(command.temporaryDir), want)
	}
	if entries, err := os.ReadDir(filepath.Join(l.base, "tmp-rel")); err != nil || len(entries) != 0 {
		t.Fatalf("a view was created below this process's directory: %v %v", entries, err)
	}
}

func TestRelativeTmpdirNativeLaunchUsesTheChildDirectory(t *testing.T) {
	l := newRelativeLaunch(t)
	entry, digest := selfNative(t, filepath.Join(l.base, "fixture"))
	command, err := NewCommandInDirectory(entry, digest, l.childDir, l.environment, helperArg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Errorf("close: %v", closeErr)
		}
	})
	command.Cmd.Dir = l.childDir
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatalf("launch with a relative TMPDIR and a different working directory: %v", err)
	}
	if report.Marker != digest || report.Argv0 != entry {
		t.Fatalf("report %+v: want marker %s and argv0 %s", report, digest, entry)
	}
	l.requireViewInChildTmp(t, command)
}

func TestRelativeTmpdirScriptLaunchUsesTheChildDirectory(t *testing.T) {
	l := newRelativeLaunch(t)
	shell := discoverPath(t, "sh")
	script := filepath.Join(l.base, "fixture", "entry.sh")
	digest := writeExecutable(t, script, shellFixture(shell, "SHL", false))
	command, err := NewCommandInDirectory(script, digest, l.childDir, l.environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Errorf("close: %v", closeErr)
		}
	})
	command.Cmd.Dir = l.childDir
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatalf("script launch with a relative TMPDIR and a different working directory: %v", err)
	}
	if report.Marker != "SHL" || report.Argv0 != script {
		t.Fatalf("report %+v: want marker SHL and argv0 %s", report, script)
	}
	l.requireViewInChildTmp(t, command)
}

func TestRelativeTmpdirNodeLaunchKeepsRelativeImports(t *testing.T) {
	l := newRelativeLaunch(t)
	discoverPath(t, "node") // skip when node is unavailable
	entry := nodeTree(t, filepath.Join(l.base, "pkg"), "NOD")
	digest := fileDigest(t, entry)
	environment := append(pathFor("node"), l.environment...)
	command, err := NewCommandInDirectory(entry, digest, l.childDir, environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Errorf("close: %v", closeErr)
		}
	})
	command.Cmd.Dir = l.childDir
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatalf("node launch with a relative TMPDIR and a different working directory: %v", err)
	}
	if report.Marker != "NOD" || report.Dependency != "DEP" || report.Parent != "PAR" || report.Nested != "NEST" || report.Resource != "RES" || report.Argv0 != entry {
		t.Fatalf("report %+v: relative imports or argv changed", report)
	}
	l.requireViewInChildTmp(t, command)
}

func TestRelativeTmpdirWithoutADirectoryResolvesAgainstThisProcess(t *testing.T) {
	l := newRelativeLaunch(t)
	entry, digest := selfNative(t, filepath.Join(l.base, "fixture"))
	command, err := NewCommand(entry, digest, l.environment, helperArg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Errorf("close: %v", closeErr)
		}
	})
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatal(err)
	}
	if report.Marker != digest {
		t.Fatalf("marker %s, want %s", report.Marker, digest)
	}
	if !filepath.IsAbs(command.temporaryDir) || filepath.Dir(command.temporaryDir) != filepath.Join(l.base, "tmp-rel") {
		t.Fatalf("the view %q: want an absolute path below %q", command.temporaryDir, filepath.Join(l.base, "tmp-rel"))
	}
}

// aliasedBoundary is the counterexample layout: this process runs in base, the
// child runs in childDir, TMPDIR=tmp-rel. base/tmp-rel is a symlink to an
// unrelated provider package, while childDir/tmp-rel is the child's real
// temporary directory. The raw spelling of TMPDIR must never be resolved in
// this process's directory to decide where the child's temporary boundary is.
type aliasedBoundary struct {
	base, childDir, entry, digest string
	environment                   []string
}

func newAliasedBoundary(t *testing.T) aliasedBoundary {
	t.Helper()
	base := scratch(t)
	childDir := filepath.Join(base, "child")
	if err := os.MkdirAll(filepath.Join(childDir, "tmp-rel"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := nodeTree(t, filepath.Join(base, "provider"), "NOD")
	if err := os.Symlink(filepath.Join(base, "provider"), filepath.Join(base, "tmp-rel")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	return aliasedBoundary{
		base: base, childDir: childDir, entry: entry, digest: fileDigest(t, entry),
		environment: append(pathFor("node"), "TMPDIR=tmp-rel"),
	}
}

func requireNodeReport(t *testing.T, command *Command, entry string) {
	t.Helper()
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatalf("launch lost a valid import: %v", err)
	}
	if report.Marker != "NOD" || report.Dependency != "DEP" || report.Parent != "PAR" || report.Nested != "NEST" || report.Resource != "RES" || report.Argv0 != entry {
		t.Fatalf("report %+v: relative imports or argv changed", report)
	}
}

func TestAliasedProcessTmpdirDoesNotStopMirroringOfTheChildsProviderParent(t *testing.T) {
	discoverPath(t, "node") // skip when node is unavailable
	a := newAliasedBoundary(t)
	t.Run("direct-control", func(t *testing.T) {
		// The fixture itself is sound: node runs the original entry directly.
		cmd := exec.Command(discoverPath(t, "node"), a.entry)
		cmd.Dir = a.childDir
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("direct launch of the original entry failed: %v: %s", err, output)
		}
		if report, parseErr := parseReport(output); parseErr != nil || report.Parent != "PAR" {
			t.Fatalf("direct launch report %+v (%v)", report, parseErr)
		}
	})
	t.Run("private", func(t *testing.T) {
		command, err := NewCommandInDirectory(a.entry, a.digest, a.childDir, a.environment)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := command.Close(); closeErr != nil {
				t.Errorf("close: %v", closeErr)
			}
		})
		command.Cmd.Dir = a.childDir
		requireNodeReport(t, command, a.entry)
	})
}

// TestChildTemporaryDirectoryStillStopsMirroring is the genuine boundary: the
// provider lives inside the child's real relative TMPDIR, whose own contents
// must not be enumerated into the view, while the package below it still is.
func TestChildTemporaryDirectoryStillStopsMirroring(t *testing.T) {
	discoverPath(t, "node") // skip when node is unavailable
	for _, mode := range []string{"private"} {
		t.Run(mode, func(t *testing.T) {
			base := scratch(t)
			childDir := filepath.Join(base, "child")
			childTmp := filepath.Join(childDir, "tmp-rel")
			entry := nodeTree(t, filepath.Join(childTmp, "pkg"), "NOD")
			if err := os.WriteFile(filepath.Join(childTmp, "canary"), []byte("unrelated"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(base)
			request := CommandRequest{Executable: entry, SHA256: fileDigest(t, entry), Directory: childDir, Environment: append(pathFor("node"), "TMPDIR=tmp-rel")}
			var command *Command
			var err error
			if command, err = NewCommandFromRequest(request); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if closeErr := command.Close(); closeErr != nil {
					t.Errorf("close: %v", closeErr)
				}
			})
			command.Cmd.Dir = childDir
			requireNodeReport(t, command, entry)
			mirroredCanary := filepath.Join(command.temporaryDir, "executable", childTmp, "canary")
			if _, err := os.Lstat(mirroredCanary); err == nil {
				t.Fatalf("%s was mirrored: the child's temporary directory was enumerated into the view", mirroredCanary)
			}
		})
	}
}
