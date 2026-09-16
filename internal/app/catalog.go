package app

import (
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/antigravity"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/provider/codex"
	"github.com/hishamkaram/delegation-layer/internal/provider/opencode"
	"github.com/hishamkaram/delegation-layer/internal/provider/pi"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NativeCatalog constructs the production registration once per command
// invocation. The fixture interpreter is historical-only: it remains
// available for collection but has no preparation function and is omitted
// from discovery.
func NativeCatalog() commonprovider.Catalog {
	fixture, err := predicate.Default().Resolve(task.FixturePredicateRef())
	if err != nil {
		panic(err) // the immutable built-in fixture contract is a build invariant
	}
	catalog, err := commonprovider.NewCatalog(
		antigravity.Registration(),
		codex.Registration(),
		claude.Registration(),
		pi.Registration(),
		opencode.Registration(),
		commonprovider.Registration{
			Description: commonprovider.Description{
				ID:           config.ProviderFixture,
				Discoverable: false,
			},
			Interpreters: []predicate.Interpreter{fixture},
		},
	)
	if err != nil {
		panic(err) // compiled registrations are validated before command use
	}
	return catalog
}
