package tasklifecycle

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// SupervisorOptions holds invocation configuration. The existing supervisor
// StoreOpener is supplied by application composition for native recovery.
// Lookup locates a supervisor pair that no binding records.
type SupervisorOptions struct {
	AdmissionStore *taskdir.Store // Borrowed actual Store for mutation after passive observation.
	Options        pueue.Options
	Lookup         supervisorcore.ExecutableLookup
	OpenStore      supervisorcore.StoreOpener
	Routing        supervisorcore.OwnerRouting
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
