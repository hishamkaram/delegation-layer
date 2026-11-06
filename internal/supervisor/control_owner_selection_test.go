package supervisor

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestControlOwnerSelectionPreservesExplicitMissingAndInvalidRoles(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-installed-runner")
	if got := DiscoverControlOwnerExecutable(missing); got != missing {
		t.Fatalf("missing explicit role became optional: %q", got)
	}
	invalid := filepath.Join(t.TempDir(), "invalid-installed-runner")
	if err := os.Mkdir(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverControlOwnerExecutable(invalid); got != invalid {
		t.Fatalf("invalid explicit role became optional: %q", got)
	}
}

func TestControlOwnerSelectionUsesLoadedRoleAndRefusesHistoricalCopy(t *testing.T) {
	directory := supervisorfixture.CanonicalPath(t, t.TempDir())
	self := filepath.Join(directory, "differently-named-current-worker")
	selectionExecutable(t, self)
	managed := &debug.BuildInfo{Path: managedRunnerMain}
	if got := discoverControlOwnerForImage(self, managed); got != self {
		t.Fatalf("loaded managed role not selected: %q", got)
	}
	old := filepath.Join(t.TempDir(), ".supervisor", "delegate-run-"+strings.Repeat("a", 64))
	selectionExecutable(t, old)
	if got := discoverControlOwnerForImage(old, managed); got != "" {
		t.Fatalf("old copy selected for new command: %q", got)
	}
}

func TestControlOwnerOptionalDiscoveryDistinguishesAbsenceFromInvalidMetadata(t *testing.T) {
	directory := t.TempDir()
	self := filepath.Join(directory, "delegate")
	if got := discoverControlOwnerForImage(self, nil); got != "" {
		t.Fatalf("absent optional role selected: %q", got)
	}
	candidate := filepath.Join(directory, "delegate-run")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := discoverControlOwnerForImage(self, nil); got != candidate {
		t.Fatalf("invalid metadata collapsed to absence: %q", got)
	}
}
