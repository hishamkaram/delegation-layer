package installation

import (
	"os"
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestBundleObservationUsesInstalledRolesAndRefusesPartialPairFallback(t *testing.T) {
	root, bin, libexec := bundleLayoutFixture(t)
	self := filepath.Join(bin, "delegate")
	aliasDirectory := t.TempDir()
	alias := filepath.Join(aliasDirectory, "delegate")
	if err := os.Symlink(self, alias); err != nil {
		t.Fatal(err)
	}
	roles := ObserveBundle(alias)
	if roles["delegate"].Path != self || roles["delegate_run"].Path != filepath.Join(bin, "delegate-run") {
		t.Fatalf("Homebrew alias lost loaded installation: %+v", roles)
	}

	for _, name := range []string{"pueue", "pueued"} {
		if role := roles[name]; role.Err != nil || role.Path != filepath.Join(libexec, name) {
			t.Fatalf("libexec role %s: %+v", name, role)
		}
	}
	// A broken selected pair must not be repaired with a different installation.
	if err := os.Symlink(filepath.Join(root, "absent"), filepath.Join(bin, "pueue")); err != nil {
		t.Fatal(err)
	}
	roles = ObserveBundle(self)
	for _, name := range []string{"pueue", "pueued"} {
		if role := roles[name]; BundleRoleStatus(role.Err) != "missing" || role.Path != filepath.Join(bin, name) {
			t.Fatalf("partial pair fell back for %s: %+v", name, role)
		}
	}
	if err := os.Remove(filepath.Join(bin, "pueue")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "pueue"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := BundleRoleStatus(ObserveBundle(self)["pueue"].Err); got != "unusable" {
		t.Fatalf("invalid role status=%s", got)
	}
}

func bundleLayoutFixture(t *testing.T) (root, bin, libexec string) {
	t.Helper()
	root = supervisorfixture.CanonicalPath(t, t.TempDir())
	bin, libexec = filepath.Join(root, "bin"), filepath.Join(root, "libexec")
	for _, directory := range []string{bin, libexec} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(bin, "delegate"), filepath.Join(bin, "delegate-run"), filepath.Join(libexec, "pueue"), filepath.Join(libexec, "pueued")} {
		selectionExecutable(t, path)
	}

	return root, bin, libexec
}
