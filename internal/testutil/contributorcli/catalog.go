// Package contributorcli composes the compiled command used by the provider
// contributor acceptance exercise. It intentionally has no production
// provider registrations in the baseline.
package contributorcli

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider"
)

// NewCatalog is the one registration composition point for this test command.
// The contributor exercise adds its test-only registration here while leaving
// the application and execution layers unchanged.
func NewCatalog() (commonprovider.Catalog, error) {
	return commonprovider.NewCatalog(contributorprovider.Registration())
}
