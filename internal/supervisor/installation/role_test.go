package installation

import (
	"runtime/debug"
	"testing"
)

func TestManagedRunnerBuildInfoUsesExecutablePath(t *testing.T) {
	if !ManagedRunnerBuildInfo(&debug.BuildInfo{Path: managedRunnerMain}) {
		t.Fatal("official delegate-run executable was not recognized")
	}
	if ManagedRunnerBuildInfo(&debug.BuildInfo{
		Path: "github.com/hishamkaram/delegation-layer",
		Main: debug.Module{Path: managedRunnerMain},
	}) {
		t.Fatal("module path was incorrectly treated as the executable package path")
	}
}
