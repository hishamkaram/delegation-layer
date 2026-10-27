package app

import "github.com/hishamkaram/delegation-layer/internal/testutil/resourcecheck"

// fixtureResources is the explicit deterministic resource checker of fixture
// dependencies. Fixture dispatches measure Delegate behavior, not host load.
var fixtureResources = resourcecheck.Clear()
