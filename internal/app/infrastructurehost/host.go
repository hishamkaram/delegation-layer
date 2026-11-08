package infrastructurehost

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// StoreAccess preserves native store opening and caller-owned cleanup custody.
type StoreAccess interface {
	OpenNative(context.Context, string) (*taskdir.Store, error)
	RetainsStore(error, *taskdir.Store) bool
}

// Migration reuses the public migration policy before closing its transition.
type Migration interface {
	MigrateOwn(context.Context, *taskdir.Store, installation.ExecutableLookup, pueue.Options, *infrastructure.PreparationObservation) (infrastructure.ManualMigrationReport, error)
}

// Host supplies fixed native composition; protocol records retain authority.
type Host struct {
	callbacks Callbacks
	stores    StoreAccess
	migration Migration
}

func NewHost(callbacks Callbacks, stores StoreAccess, migration Migration) Host {
	return Host{callbacks: callbacks, stores: stores, migration: migration}
}

func NewRouting(invocation *Invocation, defaultRunner string) Routing {
	return Routing{invocation: invocation, defaultRunner: defaultRunner}
}

func (i *Invocation) Origin() (infrastructure.Origin, bool) { return i.source.Origin() }
func (i *Invocation) Err() error                            { return i.err }
