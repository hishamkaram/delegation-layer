package app

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Keep the historical app names as aliases for the common consumer contract.
// Existing acceptance fixtures can continue to inject profiles while the
// production path selects them from the explicit catalog.
type (
	PreparedProfile = commonprovider.PreparedProfile
	PrepareProfile  = commonprovider.PrepareProfile
)

var ErrProfileUnavailable = commonprovider.ErrProfileUnavailable

// NativeProfile is the compatibility entry point for callers that need the
// production provider profile without constructing the app dependencies manually.
func NativeProfile(request task.TaskRecord) (PreparedProfile, error) {
	return NativeCatalog().Prepare(request)
}
