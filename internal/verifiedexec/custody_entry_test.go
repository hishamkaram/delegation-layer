package verifiedexec

// One captured entrypoint authority: the snapshot that will execute is the
// snapshot that is classified. These tests rewrite the ORIGINAL in place after
// Open has verified it and before the builder reads it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	oldScriptOutput = "OLD"
)

// entryFixture is an owned script whose interpreter is an owned copy of a
// shell named sh, outside every canonical system path.
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

// TestPrivateRewriteIsRefusedByCopyHash are the controls: when the
// bytes are copied rather than linked from a cache, a rewrite after Open is
// already refused by the copy's own digest check.
func TestPrivateRewriteIsRefusedByCopyHash(t *testing.T) {
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
