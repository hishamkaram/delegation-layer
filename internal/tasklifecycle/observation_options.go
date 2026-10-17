package tasklifecycle

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

// SupervisorOptions holds invocation configuration. The existing supervisor
// StoreOpener is supplied by application composition for native recovery.
type SupervisorOptions struct {
	Options           pueue.Options
	InitialExecutable string
	OpenStore         supervisorcore.StoreOpener
}

type CollectionOptions struct {
	Watch             time.Duration
	ObserveSupervisor bool
	Supervisor        SupervisorOptions
	Catalog           commonprovider.Catalog
	Registry          func() predicate.Registry
	// ReportMaintenanceDeferral receives an observed refusal within its polling
	// attempt, before a later attempt replaces the derived observation fields.
	ReportMaintenanceDeferral func(error)
}

const RunnerReceiptPollInterval = 10 * time.Millisecond
