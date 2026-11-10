package verifiedexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPoolInvocationReportersStayIsolatedAcrossRealReleaseFaults(t *testing.T) {
	rig := newPoolRig(t)
	// An abandoned stage that is a nonempty directory causes a real reclaim
	// failure without relying on UID-specific permission bypass behavior.
	stage := filepath.Join(rig.pool.imagesPath(), poolStagePrefix+"blocked")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "owned-fixture-data"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var firstReports, secondReports []error
	first := rig.pool.WithFailureReporter(func(err error) { firstReports = append(firstReports, err) })
	second := rig.pool.WithFailureReporter(func(err error) { secondReports = append(secondReports, err) })
	counts := func() [3]int {
		return [3]int{len(firstReports), len(secondReports), len(rig.logged())}
	}
	first = first.WithFailureReporter(nil)
	if err := first.ReleaseOwner(t.Context(), rig.ns("already-closed")); err != nil {
		t.Fatalf("best-effort reclaim changed successful release: %v", err)
	}
	if counts() != [3]int{1, 0, 0} {
		t.Fatalf("first invocation crossed reporters: first=%v second=%v default=%v", firstReports, secondReports, rig.logged())
	}
	if !strings.Contains(firstReports[0].Error(), "idle image reclaim failed") {
		t.Fatalf("real reclaim fault was hidden: %v", firstReports)
	}
	if err := second.ReleaseOwner(t.Context(), rig.ns("already-closed")); err != nil {
		t.Fatal(err)
	}
	if counts() != [3]int{1, 1, 0} {
		t.Fatalf("second invocation crossed reporters: first=%v second=%v default=%v", firstReports, secondReports, rig.logged())
	}
	if err := rig.pool.ReleaseOwner(t.Context(), rig.ns("already-closed")); err != nil {
		t.Fatal(err)
	}
	if counts() != [3]int{1, 1, 1} {
		t.Fatalf("derived reporter mutated original pool: first=%v second=%v default=%v", firstReports, secondReports, rig.logged())
	}
	if rig.pending("already-closed") {
		t.Fatal("a telemetry fault fabricated custody")
	}
}
