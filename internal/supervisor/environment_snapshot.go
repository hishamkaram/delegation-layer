package supervisor

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// OptionsForCurrentEnvironment is the standalone form of OptionsForEnvironment:
// it captures the process environment once as the launch scope. An explicit
// control environment, including an empty one, is copied unchanged and never
// falls back to the process environment.
func OptionsForCurrentEnvironment(base pueue.Options) pueue.Options {
	return OptionsForEnvironment(base, os.Environ())
}
