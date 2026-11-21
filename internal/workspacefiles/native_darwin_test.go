//go:build darwin

package workspacefiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"golang.org/x/sys/unix"
)

// The test executable supplies the same finite child owner as the installed
// Native executable. This exercises getconf through the real process boundary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == guardian.LaunchGateMode {
		if err := guardian.RunLaunchGate(os.Args[2:], unix.Exec); err != nil {
			os.Exit(31)
		}
		os.Exit(0)
	}
	if code, handled := execution.RunCommandOwnerMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestDarwinAutomaticUserAndSharedTempsUseTrustedNativeLookup(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := Execute(ctx, "", Scopes, Request{})
	must(t, err)
	shared, err := filepath.EvalSymlinks("/tmp")
	must(t, err)
	foundShared, foundUser := false, false
	for _, scope := range result.Scopes.Scopes {
		switch scope.Kind {
		case SharedTemp:
			foundShared = scope.Directory == shared
		case UserTemp:
			foundUser = filepath.IsAbs(scope.Directory) && scope.Directory != os.Getenv("TMPDIR")
		case Workspace:
			t.Fatal("OS temp discovery invented workspace approval")
		}
	}
	if !foundShared || !foundUser {
		t.Fatalf("missing automatic OS temp roots: %+v", result.Scopes)
	}
}
