package verifiedexec

// Legacy pool argument validation. The caller owns closure authorization.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestRecordedPoolRefusesRootsThatAreNotAbsoluteAndClean(t *testing.T) {
	for _, bad := range []string{"relative/pool", "/tmp/../pool", ""} {
		if _, err := NewPoolWithIdentity(bad, strings.Repeat("a", 64)); !errors.Is(err, errPoolRootInvalid) {
			t.Fatalf("NewPoolWithIdentity(%q) gave %v; want %v", bad, err, errPoolRootInvalid)
		}
	}
}

func mustNamespace(t *testing.T, name string) Namespace {
	t.Helper()
	ns, err := NewNamespace(name)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func TestNamespaceValidation(t *testing.T) {
	for _, valid := range []string{"a", "root0-task-3f9a", "inspection_7.v2", "0123456789abcdef", strings.Repeat("a", maxNamespaceBytes)} {
		if _, err := NewNamespace(valid); err != nil {
			t.Fatalf("%q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", ".", "..", ".hidden", "a/b", "a\\b", "a..b", "a b", "a\x00b", "é", "A", "Inspection_7.v2", "aB", strings.Repeat("a", maxNamespaceBytes+1)} {
		if _, err := NewNamespace(invalid); !errors.Is(err, errNamespaceInvalid) {
			t.Fatalf("%q gave %v; want %v", invalid, err, errNamespaceInvalid)
		}
	}
	pool, err := NewPoolWithIdentity(filepath.Join(scratch(t), "pool"), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	// The namespace is judged before the identity, so even an unbound pool
	// reports the invalid namespace.

	if err = pool.ReleaseOwner(context.Background(), Namespace{}); !errors.Is(err, errNamespaceInvalid) {
		t.Fatalf("the zero namespace reached ReleaseOwner: %v", err)
	}
	if _, err = pool.ReleasePending(Namespace{}); !errors.Is(err, errNamespaceInvalid) {
		t.Fatalf("the zero namespace reached ReleasePending: %v", err)
	}
}

func TestPoolRefusesANilContextBeforeAnyMutation(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "nil-context")
	var nilContext context.Context
	rig.seedView("a", plainRequest(entry, digest))
	if err := rig.pool.ReleaseOwner(nilContext, rig.ns("a")); !errors.Is(err, errNilContext) {
		t.Fatalf("ReleaseOwner with a nil context gave %v; want %v", err, errNilContext)
	}
	if !rig.pending("a") {
		t.Fatal("a refused ReleaseOwner removed the witness")
	}
	rig.requireViewsLinkImages("a", entry)
	if len(rig.logged()) != 0 {
		t.Fatalf("a refused call ran maintenance: %q", rig.logged())
	}
}
