package browser

import (
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/testutil/goroutinecheck"
)

func TestMain(m *testing.M) {
	if code, handled := execution.RunCommandOwnerMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(goroutinecheck.Run(m))
}

func TestGoroutineLeakGuard(t *testing.T) { goroutinecheck.VerifyInstalled(t) }
