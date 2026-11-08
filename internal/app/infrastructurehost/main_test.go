package infrastructurehost

import (
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/testutil/goroutinecheck"
)

func TestMain(m *testing.M) {
	if exit, handled := pueue.RunControlOwnerMode(os.Args[1:]); handled {
		os.Exit(exit)
	}
	os.Exit(goroutinecheck.Run(m))
}

func TestGoroutineLeakGuard(t *testing.T) { goroutinecheck.VerifyInstalled(t) }
