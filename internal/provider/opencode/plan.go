package opencode

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

const (
	nativeReadOnlyAgent       = "plan"
	nativeWorkspaceWriteAgent = "build"
)

// runArguments constructs the direct non-interactive OpenCode command. The
// brief is delivered through the runner-owned stdin, so it is never copied
// into argv or into a persisted launch record. OpenCode accepts stdin when no
// positional message is supplied.
func runArguments(request task.TaskRecord) ([]string, error) {
	return runArgumentsForProfile(request, true)
}

func legacyRunArguments(request task.TaskRecord) ([]string, error) {
	return runArgumentsForProfile(request, false)
}

func runArgumentsForProfile(request task.TaskRecord, native bool) ([]string, error) {
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	arguments := []string{"run", "--format", "json", "--dir", request.CanonicalCwd}
	arguments = append(arguments, agentArguments(request.Mode, native)...)
	arguments = append(arguments, modelArguments(request)...)
	if request.Mode == ModeWorkspaceWrite {
		// OpenCode's native --auto approves actions not denied by the selected
		// build agent. It is part of the workspace-write request only.
		arguments = append(arguments, "--auto")
	}
	if prior := request.PriorSession; prior != nil {
		if prior.Provider != Provider || !validSessionID(prior.ConversationID) || task.ValidateTaskID(prior.PredecessorTaskID) != nil {
			return nil, fmt.Errorf("%w: invalid exact continuation identity", ErrUnsupportedProfile)
		}
		arguments = append(arguments, "--session", prior.ConversationID)
	}
	return arguments, nil
}

func agentArguments(mode string, native bool) []string {
	switch mode {
	case ModeReadOnly:
		if native {
			return []string{"--agent", nativeReadOnlyAgent}
		}
		return []string{"--agent", readOnlyAgentName, "--pure"}
	case ModeWorkspaceWrite:
		if native {
			return []string{"--agent", nativeWorkspaceWriteAgent}
		}
		return []string{"--agent", workspaceWriteAgentName, "--pure"}
	default:
		return nil
	}
}

func modelArguments(request task.TaskRecord) []string {
	var arguments []string
	if request.RequestedConfig.Model != "" {
		arguments = append(arguments, "--model", request.RequestedConfig.Model)
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		arguments = append(arguments, "--variant", effort)
	}
	return arguments
}

func validateRequest(request task.TaskRecord) error {
	if err := validateProviderMode(request); err != nil {
		return err
	}
	if err := validateRequestBounds(request); err != nil {
		return err
	}
	if err := validateWorkspacePath(request); err != nil {
		return err
	}
	if err := validateTaskIdentity(request); err != nil {
		return err
	}
	return validateModelOptions(request)
}

func validateProviderMode(request task.TaskRecord) error {
	if request.Provider != Provider {
		return fmt.Errorf("%w: unsupported provider", ErrUnsupportedProfile)
	}
	switch request.Mode {
	case ModeReadOnly, ModeWorkspaceWrite:
		if request.RequestedConfig.Permission != request.Mode {
			return fmt.Errorf("%w: requested permission does not match mode", ErrUnsupportedProfile)
		}
		return nil
	default:
		return fmt.Errorf("%w: OpenCode requires a supported permission mode", ErrUnsupportedProfile)
	}
}

func validateRequestBounds(request task.TaskRecord) error {
	if request.RequestedConfig.NativeTimeout != "" {
		return fmt.Errorf("%w: OpenCode has no native timeout option", ErrUnsupportedProfile)
	}
	if request.BudgetNanos <= 0 || request.BriefLength <= 0 || request.BriefLength > task.MaxBriefSize {
		return fmt.Errorf("%w: finite brief and positive budget are required", ErrUnsupportedProfile)
	}
	return nil
}

func validateWorkspacePath(request task.TaskRecord) error {
	if !filepath.IsAbs(request.CanonicalCwd) || filepath.Clean(request.CanonicalCwd) != request.CanonicalCwd {
		return fmt.Errorf("%w: workspace must be an absolute canonical path", ErrUnsupportedProfile)
	}
	return nil
}

func validateTaskIdentity(request task.TaskRecord) error {
	if err := task.ValidateTaskID(request.TaskID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return nil
}

func validateModelOptions(request task.TaskRecord) error {
	if request.RequestedConfig.Model != "" && !validArgumentText(request.RequestedConfig.Model) {
		return fmt.Errorf("%w: model contains invalid characters", ErrUnsupportedProfile)
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" && !validArgumentText(effort) {
		return fmt.Errorf("%w: effort contains invalid characters", ErrUnsupportedProfile)
	}
	return nil
}

func validArgumentText(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character == 0 || character == '\n' || character == '\r' || character == '\t' {
			return false
		}
	}
	return true
}

func cloneArguments(arguments []string) []string { return slices.Clone(arguments) }
