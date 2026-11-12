package infrastructure

import (
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestMain(m *testing.M) {
	if exit, handled := pueue.RunControlOwnerMode(os.Args[1:]); handled {
		os.Exit(exit)
	}
	os.Exit(m.Run())
}
