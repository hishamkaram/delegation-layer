package provider

import (
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RecordedEnvironment returns the launch environment an admitted task saved.
// MetaRecord.Environment and EnvironmentRecorded own that fact; adapters that
// reconstruct an existing profile consume it here instead of reading the
// process environment. recorded is false only for a legacy record that
// predates persisted launch environments, where the caller may fall back to its
// ambient environment. A recorded empty environment is still recorded, so the
// caller must reject it rather than substitute the caller's own variables.
// The returned slice is a copy.
func RecordedEnvironment(meta task.MetaRecord) (values []string, recorded bool) {
	if meta.Environment == nil && !meta.EnvironmentRecorded {
		return nil, false
	}
	return slices.Clone(meta.Environment), true
}
