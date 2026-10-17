package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func OptionsWithEnvironment(base pueue.Options, providerEnvironment []string) pueue.Options {
	if len(providerEnvironment) == 0 {
		return base
	}
	var merged []string
	if base.Environment == nil {
		merged = pueue.DefaultEnvironment()
	} else {
		// Preserve the caller's explicit empty environment. A nil slice means
		// "use the client default"; a non-nil empty slice intentionally means
		// "launch without inherited variables".
		merged = make([]string, len(base.Environment))
		copy(merged, base.Environment)
	}
	base.Environment = mergeProviderEnvironment(merged, providerEnvironment)
	return base
}
