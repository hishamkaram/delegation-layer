package worker

import (
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/testutil/goroutinecheck"
)

// These scoped-execution tests retain the leak check from their former app owner.
func TestMain(m *testing.M) { os.Exit(goroutinecheck.Run(m)) }

func TestGoroutineLeakGuard(t *testing.T) { goroutinecheck.VerifyInstalled(t) }
