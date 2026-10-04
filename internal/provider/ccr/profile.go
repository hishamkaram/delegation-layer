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
	return prepare(request, os.Environ(), nil)
}

func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if !meta.Predicate.Equal(Reference(request.Mode)) || !meta.EnvironmentRecorded {
		return commonprovider.ProfileCandidate{}, task.ErrIdentityMismatch
	}
	var roots []string
	if meta.EffectiveConfig.Policy != nil {
		roots = meta.EffectiveConfig.Policy.WritableRoots
	}
	return prepare(request, meta.Environment, roots)
}

func arguments(request task.TaskRecord) ([]string, string, error) {
	if request.Provider != Provider || request.Mode != request.RequestedConfig.Permission || request.RequestedConfig.Model == "" {
		return nil, "", fmt.Errorf("%w: ccr:print requires --model with a CCR alias and a matching permission", commonprovider.ErrUnsupportedOption)
	}
	permission := "plan"
	if request.Mode == config.ModeWorkspaceWrite {
		permission = "bypassPermissions"
	} else if request.Mode != config.ModeReadOnly {
		return nil, "", commonprovider.ErrProfileUnavailable
	}
	if request.RequestedConfig.NativeTimeout != "" {
		return nil, "", commonprovider.ErrUnsupportedOption
	}
	args := []string{"launch", "--model", request.RequestedConfig.Model, "--auth-mode", "provider-only", "--print", "--permission-mode", permission, "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none"}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		args = append(args, "--effort", effort)
	}
	if prior := request.PriorSession; prior != nil {
		if prior.Provider != Provider || task.ValidateTaskID(prior.PredecessorTaskID) != nil {
			return nil, "", task.ErrIdentityMismatch
		}
		args = append(args, "--resume", prior.ConversationID)
	} else {
		session, err := claude.FreshSessionID(request.RootID, request.TaskID)
		if err != nil {
			return nil, "", err
		}
		args = append(args, "--session-id", session)
	}
	return args, permission, nil
}

func prepare(request task.TaskRecord, values, recordedRoots []string) (commonprovider.ProfileCandidate, error) {
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
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment, RuntimeRequirements())
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition.Arguments = []string{"launch", "--", "--help"}
	definition.Project = helpProjector(request)
	definition, _, err = definition.Snapshot()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return commonprovider.ProfileCandidate{
		Directory: request.CanonicalCwd, WritableRoots: slices.Clone(roots), Inspection: &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			args, permission, argumentErr := arguments(request)
			if argumentErr != nil {
				return commonprovider.PreparedProfile{}, argumentErr
			}
			return finalize(request, args, permission, cli, environment, roots, data)
		},
	}, nil
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

func finalize(request task.TaskRecord, args []string, permission string, cli commonprovider.CLIInfo, environment, roots []string, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, err := commonprovider.DecodeInspectionFacts(data)
	if err != nil || facts.Runtime == nil || facts.Runtime.Executable != cli.Path || facts.Runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, commonprovider.ErrProfileUnavailable
	}
	var native struct {
		Verified bool `json:"forwarded_flags_verified"`
	}
	if json.Unmarshal(facts.Native, &native) != nil || !native.Verified {
		return commonprovider.PreparedProfile{}, commonprovider.ErrProfileUnavailable
	}
	effective, err := commonprovider.NativeEffectiveConfig(request, roots, cli.SHA256, permission)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	profile := commonprovider.PreparedProfile{
		Plan:            execution.Plan{Executable: cli.Path, ExecutableSHA256: cli.SHA256, Arguments: slices.Clone(args), Directory: request.CanonicalCwd, Environment: slices.Clone(environment), Predicate: Reference(request.Mode)},
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
