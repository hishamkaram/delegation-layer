package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"

	// ModelsResponse is an advisory observation, never an admission allowlist.
	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type ModelsResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Provider      string `json:"provider"`
	ObservedAt    string `json:"observed_at"`
	commonprovider.ModelCatalog
}

func runModels(a Arguments, stdout io.Writer, deps Dependencies) int {
	catalog, err := discoverModels(a, deps)
	errorCode := 0
	if err != nil {
		catalog, errorCode = modelDiscoveryFailureCatalog(err)
	}
	response := ModelsResponse{SchemaVersion: OutputSchemaVersion, Command: "models", Provider: a.Provider, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), ModelCatalog: catalog}
	data, err := json.Marshal(response)
	if err != nil || len(data)+1 > MaxJSONResponseBytes {
		return 1
	}
	if a.JSON {
		_, err = fmt.Fprintln(stdout, string(data))
	} else {
		err = writeModelsHuman(stdout, response)
	}
	if err != nil {
		return 1
	}
	if errorCode == 2 {
		return 2
	}
	switch catalog.Status {
	case "available", "partial":
		return 0
	case "blocked", "unavailable":
		return 2
	default:
		return 1
	}
}

// modelDiscoveryFailureCatalog keeps a supervisor safety refusal distinct from
// a provider-model failure. It reuses dispatch's canonical failure
// classification so a new supervisor code cannot silently acquire a second
// lifecycle policy here.
func modelDiscoveryFailureCatalog(err error) (commonprovider.ModelCatalog, int) {
	errorCode := classifyCode(err, 1)
	catalog := commonprovider.ModelCatalog{Status: "failed", ReasonCode: "discovery_failed", Source: "none", Models: []commonprovider.ModelInfo{}}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, commonprovider.ErrProfileUnavailable) {
		catalog.Status, catalog.ReasonCode = "unavailable", "discovery_unavailable"
	}
	if errors.Is(err, inspection.ErrAdmissionExpired) {
		catalog.ReasonCode = "discovery_timeout"
	}

	failure := dispatchFailure(err, "not_created")
	switch failure.Code {
	case "supervisor_migration_blocked", "supervisor_migration_unsupported", "supervisor_upgrade_required":
		catalog.Status = "blocked"
		catalog.ReasonCode = failure.Code
		errorCode = 2
	}
	return catalog, errorCode
}

func writeModelsHuman(out io.Writer, response ModelsResponse) error {
	if _, err := fmt.Fprintf(out, "%s: %s (%s)\n", response.Provider, response.Status, response.ReasonCode); err != nil {
		return err
	}
	for _, model := range response.Models {
		if _, err := fmt.Fprintf(out, "%s\t%s\tefforts: %v\n", model.ID, model.Name, modelEffortsHuman(model.Efforts)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "Harness effort choices: %v (model compatibility may vary)\n", modelEffortsHuman(response.Efforts))
	return err
}

func discoverModels(a Arguments, deps Dependencies) (catalog commonprovider.ModelCatalog, resultErr error) {
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return catalog, err
	}
	if a.Cwd == "" {
		a.Cwd, err = os.Getwd()
		if err != nil {
			return catalog, err
		}
	}
	_, cwd, err := config.ValidateDirectories(root, a.Cwd)
	if err != nil {
		return catalog, err
	}
	registration, err := deps.Catalog.Lookup(a.Provider)
	if err != nil || registration.Models == nil || len(registration.Description.SupportedModes) == 0 {
		return catalog, commonprovider.ErrProfileUnavailable
	}
	store, err := openStore(root, deps.storeDependencies(), true)
	if err != nil {
		return catalog, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	return discoverModelsInStore(a, deps, store, cwd, registration.Description.SupportedModes[0])
}

func discoverModelsInStore(a Arguments, deps Dependencies, store *taskdir.Store, cwd, mode string) (result commonprovider.ModelCatalog, resultErr error) {
	id, err := randomid.NewTaskID()
	if err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	// Inspection records reuse the normalized request identity, but no task is
	// admitted and this marker is never sent to a provider.
	req, err := admission.BuildRequest(store.RootID, id, a.Provider, cwd, task.TaskConfig{Permission: mode, Budget: inspection.ModelDiscoveryTimeout.String()}, []byte("model discovery"), nil)
	if err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	candidate, err := prepareModelsCandidate(deps, store.Root, req)
	if err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	a, supervisor, queueRepairRunner, prepareErr := prepareModelDiscoverySupervisor(a, deps, store, candidate)
	supervisorOwner, err := supervisorcore.AcquireClient(supervisor, prepareErr)
	defer func() { resultErr = errors.Join(resultErr, supervisorOwner.CloseAfterPending(resultErr)) }()
	if err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	supervisor = supervisorOwner.Client()
	inspectionGroupReady := false
	if candidate.Inspection != nil {
		if groupErr := supervisorcore.EnsureInspectionGroup(store, supervisor, inspection.ModelDiscoveryTimeout); groupErr != nil {
			return commonprovider.ModelCatalog{}, fmt.Errorf("prepare inspection supervisor before model discovery: %w", groupErr)
		}
		inspectionGroupReady = true
	}
	if queueRepairRunner != "" {
		if repairErr := supervisorcore.EnsurePrivateQueuedRunnerRepair(store, supervisor, queueRepairRunner); repairErr != nil {
			return commonprovider.ModelCatalog{}, fmt.Errorf("repair queued task runner paths before model discovery: %w", repairErr)
		}
	}
	facts, _, err := admissionInspectionFacts(a, deps, store, req, candidate, supervisor, inspectionGroupReady)
	if err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	if err = json.Unmarshal(facts, &result); err != nil {
		return result, err
	}
	return result, commonprovider.ValidateModelCatalog(result)
}

func prepareModelDiscoverySupervisor(a Arguments, deps Dependencies, store *taskdir.Store, candidate commonprovider.ProfileCandidate) (Arguments, *pueue.Client, string, error) {
	a, queueRepairRunner, err := prepareModelDiscoveryExecutables(a, deps, store.Root)
	if err != nil {
		return Arguments{}, nil, "", err
	}
	options, err := admission.SupervisorOptionsForCandidate(deps.SupervisorOptions, candidate)
	if err != nil {
		return Arguments{}, nil, "", err
	}
	privateSupervisor, err := admissionUsesPrivateSupervisor(a, store.Root)
	if err != nil {
		return Arguments{}, nil, "", err
	}
	// Model discovery shares dispatch's private-supervisor bootstrap. A model
	// probe may not create a task record, but it must still migrate a changed
	// private daemon before it can inspect or start the queue.
	var supervisor *pueue.Client
	if privateSupervisor && a.savedSupervisor == nil {
		supervisor, err = preparePrivateAdmissionSupervisor(deps, store, options)
	} else {
		supervisor, err = bindInitialForAdmission(a, deps, store.Root, options, candidate.Inspection != nil)
	}
	if err != nil {
		return Arguments{}, supervisor, "", err
	}
	if a.savedSupervisor != nil && pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		if err = supervisorcore.EnsureAdmissionScheduling(store, supervisor, options); err != nil {
			return Arguments{}, supervisor, "", err
		}
	}
	return a, supervisor, queueRepairRunner, nil
}

func prepareModelDiscoveryExecutables(a Arguments, deps Dependencies, root string) (Arguments, string, error) {
	runnerOwnership := requestedRunnerOwnership(a)
	runner, err := prepareAdmissionExecutables(a, deps, root)
	if err != nil {
		return Arguments{}, "", err
	}
	queueRepairRunner, err := admissionQueueRepairRunner(a, deps, runner)
	if err != nil {
		return Arguments{}, "", err
	}
	a.runnerOwnership = runnerOwnership
	a.Runner = runner
	return a, queueRepairRunner, nil
}

func prepareModelsCandidate(deps Dependencies, root string, req task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	registration, err := deps.Catalog.Lookup(req.Provider)
	if err != nil || registration.Models == nil || registration.Prepare == nil {
		return commonprovider.ProfileCandidate{}, commonprovider.ErrProfileUnavailable
	}
	candidate, err := registration.Prepare(req)
	if err != nil {
		return candidate, err
	}
	if candidate.Inspection == nil {
		return candidate, commonprovider.ErrProfileUnavailable
	}
	models := registration.Models()
	definition := *candidate.Inspection
	definition.Revision = commonprovider.ModelsRevision
	definition.Models = &models
	definition.Arguments, definition.Runtime, definition.Project, definition.Remote = nil, nil, nil, nil
	definition, _, err = definition.Snapshot()
	if err != nil {
		return candidate, err
	}
	candidate.Inspection = &definition
	if err = candidate.ValidateStatePlacement(root); err != nil {
		return candidate, err
	}
	if candidate.Directory != req.CanonicalCwd || config.ValidateProvider(req.Provider) != nil {
		return candidate, commonprovider.ErrProfileUnavailable
	}
	return candidate, nil
}

func modelEffortsHuman(values []string) string {
	if values == nil {
		return "unknown"
	}
	if len(values) == 0 {
		return "none reported"
	}
	return strings.Join(values, ", ")
}
