package verifiedexec

// Binder-agnostic custody matrix: native, shell and Node entrypoints, and their
// interpreters, are mutated after bind (and after Start) and must keep running
// the admitted bytes. The same assertions run against the legacy private-copy
// constructors and against the pool, through a bindFunc.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// bindFunc binds request for owner and closes the command at test end.
type bindFunc func(owner string, request CommandRequest) *Command

// binderFactory builds a bindFunc whose storage lives below dir.
type binderFactory func(t *testing.T, dir string) bindFunc

// matrixKind is one entrypoint kind of the mutation matrix.
type matrixKind struct {
	name        string
	prepare     func(t *testing.T, dir string) (entry, digest string)
	replacement []byte
	wantMarker  func(digest string) string
	args        []string
	env         []string
	answer      string
	direct      func(path string) string
}

// directMarker runs a path directly, as a launcher without custody would, and
// reports the marker or the failure. It is an observation only.
func directMarker(name string, args ...string) string {
	output, err := exec.Command(name, args...).Output()
	if err != nil {
		return "exec-failed: " + err.Error()
	}
	report, err := parseReport(output)
	if err != nil {
		return "unparsable: " + err.Error()
	}
	return report.Marker
}

func matrixKinds(t *testing.T) []matrixKind {
	t.Helper()
	shell := discoverPath(t, "sh")
	return []matrixKind{
		{
			name: "native", prepare: func(t *testing.T, dir string) (string, string) { return selfNative(t, dir) },
			replacement: []byte("not a program"), wantMarker: func(digest string) string { return digest },
			args: []string{helperArg, "handshake"}, answer: "go",
			direct: func(path string) string { return directMarker(path, helperArg) },
		},
		{
			name: "shell", env: pathFor("sh"), answer: "GO", wantMarker: func(string) string { return "OLD" },
			prepare: func(t *testing.T, dir string) (string, string) {
				entry := filepath.Join(dir, "entry.sh")
				return entry, writeExecutable(t, entry, shellFixture(shell, "OLD", true))
			},
			replacement: shellFixture(shell, "NEW", true),
			direct:      func(path string) string { return directMarker(shell, path) },
		},
	}
}

// runMatrix mutates native and shell entrypoints after bind and, for the two
// mutations a running image could observe, after Start.
func runMatrix(t *testing.T, factory binderFactory) {
	for _, kind := range matrixKinds(t) {
		for _, m := range mutations {
			t.Run(kind.name+"/after-bind/"+m.name, func(t *testing.T) { matrixAfterBind(t, factory, kind, m) })
			if m.name != "truncate" {
				t.Run(kind.name+"/after-start/"+m.name, func(t *testing.T) { matrixAfterStart(t, factory, kind, m) })
			}
		}
	}
}

func matrixAfterBind(t *testing.T, factory binderFactory, kind matrixKind, m mutation) {
	dir := scratch(t)
	rec := newRecorder(t)
	entry, digest := kind.prepare(t, filepath.Join(dir, "fixture"))
	args := kind.args[:min(len(kind.args), 1)]
	command := factory(t, dir)("a", CommandRequest{Executable: entry, SHA256: digest, Environment: kind.env, Args: args})
	mutateAndConfirm(t, m, entry, kind.replacement, digest)
	rec.note("control-direct-run-of-mutated-original", "%s", kind.direct(entry))
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatalf("view did not run after the original changed: %v", err)
	}
	if want := kind.wantMarker(digest); report.Marker != want {
		t.Fatalf("child observed %q after %s of the original; want %q", report.Marker, m.name, want)
	}
}

func matrixAfterStart(t *testing.T, factory binderFactory, kind matrixKind, m mutation) {
	dir := scratch(t)
	entry, digest := kind.prepare(t, filepath.Join(dir, "fixture"))
	command := factory(t, dir)("a", CommandRequest{Executable: entry, SHA256: digest, Environment: kind.env, Args: kind.args})
	child, err := startHandshake(t, command)
	if err != nil {
		t.Fatal(err)
	}
	// The original is not the running image, so even an in-place write that a
	// running ELF would refuse (ETXTBSY) is possible here.
	mutateAndConfirm(t, m, entry, kind.replacement, digest)
	report, err := child.release()
	if err != nil {
		t.Fatal(err)
	}
	if want := kind.wantMarker(digest); report.Marker != want || report.Handshake != kind.answer {
		t.Fatalf("report %+v after %s during run; want %q after the %q handshake", report, m.name, want, kind.answer)
	}
}

// runNodeEntry checks Node ./ ../ nested imports, a resource beside the module,
// import.meta.url and the three mutations, then states a limitation as an
// assertion: an imported dependency is not frozen.
func runNodeEntry(t *testing.T, factory binderFactory) {
	node := discoverPath(t, "node")
	dir := scratch(t)
	rec := newRecorder(t)
	bind := factory(t, dir)
	entry := nodeTree(t, filepath.Join(dir, "fixture"), "OLD")
	original, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestOf(original)
	request := CommandRequest{Executable: entry, SHA256: digest, Environment: pathFor("node")}
	report, err := runToCompletion(bind("baseline", request))
	if err != nil {
		t.Fatal(err)
	}
	requireNodeResolution(t, rec, report, entry, node)
	for _, m := range mutations {
		command := bind("after-"+m.name, request)
		mutateAndConfirm(t, m, entry, nodeFixture("NEW"), digest)
		if report, err = runToCompletion(command); err != nil {
			t.Fatalf("node view after %s of the original: %v", m.name, err)
		}
		if report.Marker != "OLD" {
			t.Fatalf("node child observed %q after %s; want OLD", report.Marker, m.name)
		}
		restoreNode(t, entry, original, m)
	}
	requireDependencyNotFrozen(t, rec, bind, request)
}

// restoreNode puts the original entry back so the next mutation starts clean.
func restoreNode(t *testing.T, entry string, original []byte, m mutation) {
	t.Helper()
	if m.name == "rename-over" {
		if err := os.Remove(entry); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(entry+".moved", entry); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(entry, original, 0o700); err != nil {
		t.Fatal(err)
	}
}

func requireNodeResolution(t *testing.T, rec *recorder, report proofReport, entry, node string) {
	t.Helper()
	if report.Marker != "OLD" || report.Argv0 != entry || report.Dependency != "DEP" ||
		report.Parent != "PAR" || report.Nested != "NEST" || report.Resource != "RES" {
		t.Fatalf("node report %+v; want ./ ../ nested imports and a resource beside the module to resolve, argv0 %s", report, entry)
	}
	rec.note("node-import-meta-url", "%s", report.ImportMetaURL)
	rec.note("node-argv0", "%s", report.Argv0)
	rec.note("node-interpreter", "%s version %s", node, report.Runtime)
}

// requireDependencyNotFrozen states a limitation as an assertion: the entry
// bytes are a snapshot, the files it imports are not.
func requireDependencyNotFrozen(t *testing.T, rec *recorder, bind bindFunc, request CommandRequest) {
	t.Helper()
	command := bind("dependency", request)
	dependency := filepath.Join(filepath.Dir(request.Executable), "dep.mjs")
	if err := os.WriteFile(dependency, []byte("export const dependency = \"XYZ\";\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatal(err)
	}
	if report.Dependency != "XYZ" {
		t.Fatalf("dependency read %q: the mirror no longer follows the original installation", report.Dependency)
	}
	rec.note("limitation-dependency-not-frozen", "an imported file changed after bind was observed by the child (%s)", report.Dependency)
}

// interpreterCase is an owned copy of a host interpreter plus a script for it.
type interpreterCase struct {
	name        string
	source      string
	controlArgs []string
	prepare     func(t *testing.T, scriptDir, owned string) (entry, digest string)
}

func interpreterCases(t *testing.T) []interpreterCase {
	t.Helper()
	cases := []interpreterCase{{
		name: "node", source: discoverPath(t, "node"), controlArgs: []string{"--version"},
		prepare: func(t *testing.T, scriptDir, _ string) (string, string) {
			return nodeTree(t, scriptDir, "OLD"), digestOf(nodeFixture("OLD"))
		},
	}}
	if shell := discoverPath(t, "sh"); !useCanonicalDarwinSystemExecutable(shell) {
		cases = append(cases, interpreterCase{
			name: "sh", source: shell, controlArgs: []string{"-c", "exit 0"},
			prepare: func(t *testing.T, scriptDir, owned string) (string, string) {
				entry := filepath.Join(scriptDir, "script.sh")
				return entry, writeExecutable(t, entry, shellFixture(owned, "OLD", false))
			},
		})
	} else {
		t.Logf("sh interpreter case not run: %s is a sealed platform binary that the builder keeps canonical", shell)
	}
	return cases
}

// runInterpreterCases mutates an owned interpreter copy three ways after bind.
// The owned copy must first run directly (a failed control is a setup defect,
// reported as such); only then is it bound and mutated.
func runInterpreterCases(t *testing.T, factory binderFactory) {
	for _, c := range interpreterCases(t) {
		t.Run(c.name, func(t *testing.T) {
			dir := scratch(t)
			rec := newRecorder(t)
			bind := factory(t, dir)
			owned := ownedInterpreterCopy(t, filepath.Join(dir, "interpreter"), c.source)
			if err := directInterpreterControl(owned, c.controlArgs...); err != nil {
				t.Fatal(err)
			}
			rec.note("direct-control", "owned %s copy of %s ran by itself before binding", c.name, c.source)
			entry, digest := c.prepare(t, filepath.Join(dir, "script"), owned)
			command := bind("a", CommandRequest{Executable: entry, SHA256: digest, Environment: []string{"PATH=" + filepath.Dir(owned)}})
			before := fileDigest(t, owned)
			for _, m := range []mutation{mutations[1], mutations[2], mutations[0]} {
				mutateAndConfirm(t, m, owned, []byte("not an interpreter"), before)
			}
			report, err := runToCompletion(command)
			if err != nil {
				t.Fatalf("custody: %s view did not run after its interpreter original was mutated three ways: %v", c.name, err)
			}
			if report.Marker != "OLD" {
				t.Fatalf("custody: child observed %q", report.Marker)
			}
			rec.note("interpreter", "%s copied from %s, mutated in place, truncated, renamed over; the view still ran", c.name, c.source)
		})
	}
}

// TestCustodyOwnedInterpreterKeepsInstallationLayout is the regression for the
// Darwin setup defect: a bare copy of an interpreter that finds its library
// beside the install fails the direct control, while the layout-preserving copy
// passes it. The synthetic installation resolves ../lib the way
// @loader_path/../lib does, on every host.
func TestCustodyOwnedInterpreterKeepsInstallationLayout(t *testing.T) {
	dir := scratch(t)
	rec := newRecorder(t)
	shell := discoverPath(t, "sh")
	install := filepath.Join(dir, "install", "prefix")
	tool := filepath.Join(install, "bin", "tool")
	writeExecutable(t, tool, []byte("#!"+shell+"\n. \"${0%/*}/../lib/dep.sh\"\nexit 0\n"))
	writeExecutable(t, filepath.Join(install, "lib", "dep.sh"), []byte("DEP=1\n"))

	bare := filepath.Join(dir, "bare", "tool")
	copyFileTo(t, tool, bare)
	if err := directInterpreterControl(bare); !errors.Is(err, errSetupControl) {
		t.Fatalf("a bare copy without ../lib passed the direct control (%v); the control cannot detect the defect", err)
	}
	owned := ownedInterpreterCopy(t, filepath.Join(dir, "owned", "interpreter"), tool)
	if err := directInterpreterControl(owned); err != nil {
		t.Fatalf("the layout-preserving copy does not run by itself: %v", err)
	}
	info, err := os.Lstat(filepath.Join(filepath.Dir(owned), "..", "lib"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("lib beside the owned copy is not a symlink to the installation (%v)", err)
	}
	rec.note("layout", "bare copy fails the control; the layout-preserving copy passes it; lib is a symlink, never written through")
}
