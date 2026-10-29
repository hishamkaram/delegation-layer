package verifiedexec

// One captured entrypoint authority: the snapshot that will execute is the
// snapshot that is classified. These tests rewrite the ORIGINAL in place after
// Open has verified it and before the builder reads it, and replace the
// interpreter the script names after bind.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const (
	oldScriptOutput = "OLD"
	bypassOutput    = "BYPASS"
)

// entryFixture is an owned script whose interpreter is an owned copy of a
// shell named sh, outside every canonical system path, so the test can replace
// the interpreter at its path after bind.
type entryFixture struct {
	interpreter string
	script      string
	digest      string
}

func newEntryFixture(t *testing.T, dir string) entryFixture {
	t.Helper()
	interpreter := filepath.Join(dir, "interpreter", "sh")
	copyFileTo(t, discoverPath(t, "sh"), interpreter)
	script := filepath.Join(dir, "fixture", "entry.sh")
	digest := writeExecutable(t, script, []byte("#!"+interpreter+"\nprintf '"+oldScriptOutput+"\\n'\n"))
	return entryFixture{interpreter: interpreter, script: script, digest: digest}
}

// rewriteInPlace keeps the inode and drops the shebang, as an installer that
// rewrites the file in place would.
func (f entryFixture) rewriteInPlace(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.script, []byte("printf 'NEW\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// replaceInterpreter swaps the interpreter path for a script that prints the
// bypass marker, so a launch that reaches the replaced path is visible.
func (f entryFixture) replaceInterpreter(t *testing.T) {
	t.Helper()
	bypass := "#!" + discoverPath(t, "sh") + "\nprintf '" + bypassOutput + "\\n'\n"
	if err := os.Rename(f.interpreter, f.interpreter+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.interpreter, []byte(bypass), 0o700); err != nil {
		t.Fatal(err)
	}
}

// interpreterFormProblem asserts the command launches the materialized
// interpreter with the entry sourced as data, on every OS: the command path is
// a file named like the interpreter below the view, argv[0] is the original
// interpreter path, and the script is passed through -c. It returns "" when the
// form holds.
func interpreterFormProblem(command *Command, f entryFixture) string {
	if filepath.Base(command.Cmd.Path) != "sh" || command.Cmd.Path == f.interpreter ||
		!strings.HasPrefix(command.Cmd.Path, command.temporaryDir) {
		return "command path " + command.Cmd.Path + " is not the interpreter snapshot inside the view " + command.temporaryDir
	}
	if command.Cmd.Args[0] != f.interpreter || !slices.Contains(command.Cmd.Args, "-c") {
		return "argv " + strings.Join(command.Cmd.Args, " ") + ": want the interpreter path then -c"
	}
	return ""
}

func TestPoolCachedScriptIsNeverReclassifiedAsNative(t *testing.T) {
	rig := newPoolRig(t)
	f := newEntryFixture(t, rig.dir)
	first := rig.binder("first", plainRequest(f.script, f.digest))
	if problem := interpreterFormProblem(first, f); problem != "" {
		t.Fatalf("first bind: %s", problem)
	}

	rewrites := 0
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointSourceOpened {
			rewrites++
			f.rewriteInPlace(t)
		}
	}
	command, err := rig.pool.Bind(context.Background(), rig.ns("second"), plainRequest(f.script, f.digest))
	if rewrites != 1 {
		t.Fatalf("the source-opened point fired %d times; the test is void", rewrites)
	}
	if err != nil {
		// Refusing is an acceptable outcome; running the kernel's choice is not.
		rig.requireViewDirs("second", 0)
		rig.rec.note("cached-reclassification", "refused: %v", err)
		return
	}
	t.Cleanup(func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Errorf("close: %v", closeErr)
		}
	})
	if problem := interpreterFormProblem(command, f); problem != "" {
		t.Errorf("second bind from the cache: %s", problem)
	}
	if runtime.GOOS != "linux" {
		rig.rec.note("cached-reclassification", "interpreter form verified; runtime output not asserted on %s", runtime.GOOS)
		return
	}
	f.replaceInterpreter(t)
	var stdout strings.Builder
	command.Cmd.Stdout = &stdout
	if err = command.Cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != oldScriptOutput {
		t.Errorf("the child printed %q after the interpreter was replaced; want %q through the snapshot interpreter", got, oldScriptOutput)
	}
}

// TestColdAndPrivateRewriteAreRefusedByCopyHash are the controls: when the
// bytes are copied rather than linked from a cache, a rewrite after Open is
// already refused by the copy's own digest check.
func TestColdAndPrivateRewriteAreRefusedByCopyHash(t *testing.T) {
	t.Run("pool-cold-miss", func(t *testing.T) {
		rig := newPoolRig(t)
		f := newEntryFixture(t, rig.dir)
		rig.pool.faults.at = func(point poolPoint, _ string) {
			if point == pointSourceOpened {
				f.rewriteInPlace(t)
			}
		}
		_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(f.script, f.digest))
		if err == nil || !strings.Contains(err.Error(), "does not match admission") {
			t.Fatalf("a cold bind of a rewritten original gave %v", err)
		}
		rig.requireViewDirs("a", 0)
	})
	t.Run("private-copy", func(t *testing.T) {
		dir := scratch(t)
		f := newEntryFixture(t, dir)
		file, err := Open(f.script, f.digest)
		if err != nil {
			t.Fatal(err)
		}
		f.rewriteInPlace(t)
		binding := privateBinding(f.script, f.digest, "", []string{"TMPDIR=" + dir}, nil)
		if _, err = binding.newCommand(file); err == nil || !strings.Contains(err.Error(), "does not match admission") {
			t.Fatalf("a private bind of a rewritten original gave %v", err)
		}
	})
}

func TestNestedScriptInterpreterIsRefusedOnTheBoundInterpreter(t *testing.T) {
	dir := scratch(t)
	inner := filepath.Join(dir, "interpreter", "sh")
	writeExecutable(t, inner, []byte("#!"+discoverPath(t, "sh")+"\nexit 0\n"))
	script := filepath.Join(dir, "fixture", "entry.sh")
	digest := writeExecutable(t, script, []byte("#!"+inner+"\nprintf x\n"))
	if _, err := NewCommand(script, digest, []string{"TMPDIR=" + dir}); err == nil || !strings.Contains(err.Error(), "nested verified script interpreters are unsupported") {
		t.Fatalf("a nested script interpreter gave %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "delegation-layer-verified-*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("a refused nested interpreter left views %v (%v)", entries, err)
	}
}
