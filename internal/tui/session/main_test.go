package session

import (
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/testutil/goroutinecheck"
)

func TestMain(m *testing.M)                    { os.Exit(goroutinecheck.Run(m)) }
func TestGoroutineCheckInstalled(t *testing.T) { goroutinecheck.VerifyInstalled(t) }
