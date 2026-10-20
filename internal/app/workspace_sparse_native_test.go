package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestNativeProvisionFullLinkedSourceUnderSparseMain(t *testing.T) {
	for _, attachment := range []string{"attached", "detached"} {
		t.Run(attachment, func(t *testing.T) {
			lineage, source := nativeSparseSource(t, true, attachment)
			_, registration := lineage.provisionedChild(t, filepath.Join(source, "lib"))
			assertNativeSourceInheritance(t, lineage, source, registration, "lib", false)
		})
	}
}

func TestNativeProvisionSparseLinkedSourceUnderFullMain(t *testing.T) {
	for _, attachment := range []string{"attached", "detached"} {
		t.Run(attachment, func(t *testing.T) {
			lineage, source := nativeSparseSource(t, false, attachment)
			_, registration := lineage.provisionedChild(t, filepath.Join(source, "docs"))
			assertNativeSourceInheritance(t, lineage, source, registration, "docs", true)
		})
	}
}

func nativeSparseSource(t *testing.T, mainSparse bool, attachment string) (nativeLineage, string) {
	t.Helper()
	lineage := newNativeLineage(t)
	for _, directory := range []string{"docs", "lib"} {
		path := filepath.Join(lineage.main, directory)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		writeLineageFile(t, filepath.Join(path, "fixture.txt"), directory+"\n")
	}
	lineage.gitIn(t, lineage.main, "add", "-A")
	lineage.commit(t, lineage.main, "sparse fixture")
	lineage.gitIn(t, lineage.main, "sparse-checkout", "set", "--cone", "docs")
	if !mainSparse {
		lineage.gitIn(t, lineage.main, "sparse-checkout", "disable")
	}
	lineage.gitIn(t, lineage.main, "config", "--worktree", "delegate.fixtureContext", "main")
	source := lineage.linkedSource(t, filepath.Join(lineage.root, "source"))
	if mainSparse {
		lineage.gitIn(t, source, "sparse-checkout", "disable")
	} else {
		lineage.gitIn(t, source, "sparse-checkout", "set", "--cone", "docs")
	}
	if attachment == "attached" {
		lineage.gitIn(t, source, "switch", "--quiet", "-c", "fixture-source")
	}
	lineage.gitIn(t, source, "config", "--worktree", "delegate.fixtureContext", "source")
	return lineage, source
}

func assertNativeSourceInheritance(t *testing.T, lineage nativeLineage, source string, registration workspace.Registration, relative string, sparse bool) {
	t.Helper()
	checkout := registration.CheckoutPath
	if registration.SourceRoot != source || registration.RelativeCwd != relative || registration.CanonicalCwd != filepath.Join(checkout, relative) {
		t.Fatalf("child did not preserve source and mapped subdirectory: %+v", registration)
	}
	wantSparse := "false"
	if sparse {
		wantSparse = "true"
	}
	if got := lineage.gitIn(t, checkout, "config", "--worktree", "--bool", "core.sparseCheckout"); got != wantSparse {
		t.Fatalf("child sparse-checkout = %q, want source value %q", got, wantSparse)
	}
	if got := lineage.gitIn(t, checkout, "config", "--worktree", "delegate.fixtureContext"); got != "source" {
		t.Fatalf("child inherited per-worktree context %q, want source", got)
	}
	if sparse {
		assertPathAbsent(t, filepath.Join(checkout, "lib"))
	} else if data, err := os.ReadFile(filepath.Join(checkout, "lib", "fixture.txt")); err != nil || string(data) != "lib\n" {
		t.Fatalf("full source's lib file missing or changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(checkout, "docs", "fixture.txt")); err != nil || string(data) != "docs\n" {
		t.Fatalf("source's docs file missing or changed: %q, %v", data, err)
	}
}
