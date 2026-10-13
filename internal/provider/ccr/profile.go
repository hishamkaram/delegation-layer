package ccr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepare(request, os.Environ(), nil, task.PredicateRef{})
}

func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if !meta.EnvironmentRecorded {
		return commonprovider.ProfileCandidate{}, task.ErrIdentityMismatch
	}
	var roots []string
	if meta.EffectiveConfig.Policy != nil {
		roots = meta.EffectiveConfig.Policy.WritableRoots
	}
	return prepare(request, meta.Environment, roots, meta.Predicate)
}

func arguments(request task.TaskRecord) ([]string, string, error) {
	return argumentsForPreparation(request, resumeRoutePreparation)
}

func argumentsForPreparation(request task.TaskRecord, revision preparationRevision) ([]string, string, error) {
	permission, err := permissionFor(request)
	if err != nil {
		return nil, "", err
	}
	if request.RequestedConfig.NativeTimeout != "" {
		return nil, "", commonprovider.ErrUnsupportedOption
	}
	args, err := launchPrefix(request, revision)
	if err != nil {
		return nil, "", err
	}
	args = append(args, "--print", "--permission-mode", permission, "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none")
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		args = append(args, "--effort", effort)
	}
	args, err = appendSessionArgument(request, args)
	if err != nil {
		return nil, "", err
	}
	return args, permission, nil
}

func permissionFor(request task.TaskRecord) (string, error) {
	if request.Provider != Provider || request.Mode != request.RequestedConfig.Permission {
		return "", fmt.Errorf("%w: ccr:print requires --model with a CCR alias and a matching permission", commonprovider.ErrUnsupportedOption)
	}
	switch request.Mode {
	case config.ModeReadOnly:
		return "plan", nil
	case config.ModeWorkspaceWrite:
		return "bypassPermissions", nil
	default:
		return "", commonprovider.ErrProfileUnavailable
	}
}

func launchPrefix(request task.TaskRecord, revision preparationRevision) ([]string, error) {
	prior := request.PriorSession
	if prior == nil {
		return modeledLaunchPrefix(request)
	}
	if prior.Provider != Provider || task.ValidateTaskID(prior.PredecessorTaskID) != nil {
		return nil, task.ErrIdentityMismatch
	}
	if revision == resumeRoutePreparation {
		// CCR retains the route selected by the exact session. Continue through
		// its dedicated restore path. Delegate does not reconstruct model or auth
		// flags from current configuration, which could rediscover a provider or
		// select first-party authentication instead of the recorded provider route.
		return []string{"launch", "--resume-route"}, nil
	}
	return modeledLaunchPrefix(request)
}

func modeledLaunchPrefix(request task.TaskRecord) ([]string, error) {
	if request.RequestedConfig.Model == "" {
		return nil, fmt.Errorf("%w: ccr:print requires --model with a CCR alias", commonprovider.ErrUnsupportedOption)
	}
	return []string{"launch", "--model", request.RequestedConfig.Model, "--auth-mode", "provider-only"}, nil
}

func appendSessionArgument(request task.TaskRecord, args []string) ([]string, error) {
	if prior := request.PriorSession; prior != nil {
		return append(args, "--resume", prior.ConversationID), nil
	}
	session, err := claude.FreshSessionID(request.RootID, request.TaskID)
	if err != nil {
		return nil, err
	}
	return append(args, "--session-id", session), nil
}

func prepare(request task.TaskRecord, values, recordedRoots []string, reference task.PredicateRef) (commonprovider.ProfileCandidate, error) {
	environment, roots, err := nativeEnvironment(values)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if recordedRoots != nil {
		roots = slices.Clone(recordedRoots)
	}
	cli, err := commonprovider.LocateCLI("ccr")
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment, runtimeRequirements())
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition, err = inspectionDefinition(request, reference, definition)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return commonprovider.ProfileCandidate{
		Directory: request.CanonicalCwd, WritableRoots: slices.Clone(roots), Inspection: &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			revision, selectedReference, selectionErr := selectPreparation(reference, request.Mode, data)
			if selectionErr != nil {
				return commonprovider.PreparedProfile{}, selectionErr
			}
			args, permission, argumentErr := argumentsForPreparation(request, revision)
			if argumentErr != nil {
				return commonprovider.PreparedProfile{}, argumentErr
			}
			return finalize(request, args, permission, cli, environment, roots, selectedReference, data)
		},
	}, nil
}

const routeInspectionRevision = "ccr-route-capability-v1"

type routeCapabilities struct {
	ResumeRoute bool `json:"resume_route"`
}

func inspectionDefinition(request task.TaskRecord, reference task.PredicateRef, definition commonprovider.InspectionDefinition) (commonprovider.InspectionDefinition, error) {
	if reference != (task.PredicateRef{}) {
		contract, err := preparationForReference(reference, request.Mode)
		if err != nil {
			return commonprovider.InspectionDefinition{}, err
		}
		if contract.inspection == legacyInspection {
			return legacyInspectionDefinition(request, definition)
		}
	}
	return routeInspectionDefinition(definition)
}

// legacyInspectionDefinition retains the exact inspection proof shape used by
// CCR foreground v1 and v2 tasks. It is selected only from their stored
// predicate, so current tasks cannot be downgraded to the historical contract.
func legacyInspectionDefinition(request task.TaskRecord, definition commonprovider.InspectionDefinition) (commonprovider.InspectionDefinition, error) {
	definition.Arguments = []string{"launch", "--", "--help"}
	definition.Project = helpProjector(request)
	snapshot, _, err := definition.Snapshot()
	return snapshot, err
}

func routeInspectionDefinition(definition commonprovider.InspectionDefinition) (commonprovider.InspectionDefinition, error) {
	definition.Revision = routeInspectionRevision
	definition.Arguments = []string{"launch", "--help"}
	definition.Project = func(output []byte) (json.RawMessage, error) {
		return task.MarshalCanonical(routeCapabilities{ResumeRoute: commonprovider.ContainsCLIFlag(output, "--resume-route")})
	}
	snapshot, _, err := definition.Snapshot()
	return snapshot, err
}

func selectPreparation(reference task.PredicateRef, mode string, data json.RawMessage) (preparationRevision, task.PredicateRef, error) {
	if reference == (task.PredicateRef{}) {
		capabilities, err := routeCapabilitiesForFacts(data)
		if err != nil {
			return 0, task.PredicateRef{}, err
		}
		if capabilities.ResumeRoute {
			return resumeRoutePreparation, Reference(mode), nil
		}
		return legacyPreparation, modeledReference(mode), nil
	}
	contract, err := preparationForReference(reference, mode)
	if err != nil {
		return 0, task.PredicateRef{}, err
	}
	if contract.inspection == legacyInspection {
		if !legacyForwardedFlagsVerified(data) {
			return 0, task.PredicateRef{}, commonprovider.ErrProfileUnavailable
		}
		return contract.launch, reference, nil
	}
	capabilities, err := routeCapabilitiesForFacts(data)
	if err != nil || capabilities.ResumeRoute != (contract.launch == resumeRoutePreparation) {
		return 0, task.PredicateRef{}, commonprovider.ErrProfileUnavailable
	}
	return contract.launch, reference, nil
}

func routeCapabilitiesForFacts(data json.RawMessage) (routeCapabilities, error) {
	native, err := inspectionNativeFacts(data)
	if err != nil {
		return routeCapabilities{}, err
	}
	var capabilities routeCapabilities
	if err = task.DecodeStrict(native, &capabilities); err != nil {
		return routeCapabilities{}, commonprovider.ErrProfileUnavailable
	}
	return capabilities, nil
}

func legacyForwardedFlagsVerified(data json.RawMessage) bool {
	native, err := inspectionNativeFacts(data)
	if err != nil {
		return false
	}
	var facts struct {
		Verified bool `json:"forwarded_flags_verified"`
	}
	return json.Unmarshal(native, &facts) == nil && facts.Verified
}

func inspectionNativeFacts(data json.RawMessage) (json.RawMessage, error) {
	facts, err := commonprovider.DecodeInspectionFacts(data)
	if err != nil || facts.Runtime == nil || len(facts.Native) == 0 {
		return nil, commonprovider.ErrProfileUnavailable
	}
	return facts.Native, nil
}

func helpProjector(request task.TaskRecord) func([]byte) (json.RawMessage, error) {
	flags := []string{"--input-format", "--output-format", "--verbose", "--permission-prompts", "--permission-mode", "--session-id", "--resume"}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		flags = append(flags, "--effort")
	}
	return func(help []byte) (json.RawMessage, error) {
		for _, flag := range flags {
			if !commonprovider.ContainsCLIFlag(help, flag) {
				return nil, fmt.Errorf("%w: missing forwarded flag %s", commonprovider.ErrProfileUnavailable, flag)
			}
		}
		return json.RawMessage(`{"forwarded_flags_verified":true}`), nil
	}
}

func finalize(request task.TaskRecord, args []string, permission string, cli commonprovider.CLIInfo, environment, roots []string, reference task.PredicateRef, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, err := commonprovider.DecodeInspectionFacts(data)
	if err != nil || facts.Runtime == nil || facts.Runtime.Executable != cli.Path || facts.Runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, commonprovider.ErrProfileUnavailable
	}
	effective, err := commonprovider.NativeEffectiveConfig(request, roots, cli.SHA256, permission)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	profile := commonprovider.PreparedProfile{
		Plan:            execution.Plan{Executable: cli.Path, ExecutableSHA256: cli.SHA256, Arguments: slices.Clone(args), Directory: request.CanonicalCwd, Environment: slices.Clone(environment), Predicate: reference},
		ObservedVersion: facts.Runtime.Version, Effective: effective, WritableRoots: slices.Clone(roots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return claude.NewStreamIdentityObserver(Provider, request.RootID, request.TaskID, expected, record)
		},
	}
	if err = profile.Validate(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return profile, nil
}

func nativeEnvironment(values []string) ([]string, []string, error) {
	environment, roots, err := claude.NativeEnvironment(values)
	if err != nil {
		return nil, nil, err
	}
	entries := make(map[string]string)
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		entries[key] = value
	}
	data := entries["XDG_DATA_HOME"]
	if data == "" {
		data = filepath.Join(entries["HOME"], ".local", "share")
	}
	root, err := config.CanonicalizePath(filepath.Join(data, "claude-code-router"))
	if err != nil {
		return nil, nil, err
	}
	roots = append(roots, root)
	slices.Sort(roots)
	return environment, slices.Compact(roots), nil
}
