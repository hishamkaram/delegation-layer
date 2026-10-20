package opencode

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

const (
	nativeReadOnlyAgent       = "plan"
	nativeWorkspaceWriteAgent = "build"
	modelVariantSeparator     = "#"
)

// runArguments constructs the non-interactive OpenCode command for the
// admitted dialect. The brief is delivered through the runner-owned stdin, so
// it is never copied into argv or into a persisted launch record. The ACP
// dialect launches `opencode acp` alone: the workspace, agent, model, effort
// and exact session travel in its protocol requests, and Plan.Directory owns
// the process directory. The run dialects accept the brief on stdin when no
// positional message is supplied.
func runArguments(request task.TaskRecord, dialect launchDialect) ([]string, error) {
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	if dialect == dialectACP {
		if err := validatePriorSession(request); err != nil {
			return nil, err
		}
		return []string{"acp"}, nil
	}
	arguments := []string{"run", "--format", "json"}
	if dialect == dialectNativeV2 {
		// A private server keeps native tool execution in the supervised tree.
		arguments = append(arguments, "--standalone")
	} else {
		arguments = append(arguments, "--dir", request.CanonicalCwd)
	}
	arguments = append(arguments, agentArguments(request.Mode, dialect.native())...)
	model, err := modelArguments(request, dialect)
	if err != nil {
		return nil, err
	}
	arguments = append(arguments, model...)
	if request.Mode == ModeWorkspaceWrite {
		// OpenCode's native --auto approves actions not denied by the selected
		// build agent. It is part of the workspace-write request only.
		arguments = append(arguments, "--auto")
	}
	if err := validatePriorSession(request); err != nil {
		return nil, err
	}
	if prior := request.PriorSession; prior != nil {
		arguments = append(arguments, "--session", prior.ConversationID)
	}
	return arguments, nil
}

// validatePriorSession checks an exact continuation identity, when present.
func validatePriorSession(request task.TaskRecord) error {
	prior := request.PriorSession
	if prior != nil && (prior.Provider != Provider || !validSessionID(prior.ConversationID) || task.ValidateTaskID(prior.PredecessorTaskID) != nil) {
		return fmt.Errorf("%w: invalid exact continuation identity", ErrUnsupportedProfile)
	}
	return nil
}

// nativeAgent returns the native primary agent selected by a permission mode.
func nativeAgent(mode string) (string, error) {
	switch mode {
	case ModeReadOnly:
		return nativeReadOnlyAgent, nil
	case ModeWorkspaceWrite:
		return nativeWorkspaceWriteAgent, nil
	default:
		return "", fmt.Errorf("%w: unsupported OpenCode native permission mode", ErrUnsupportedProfile)
	}
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

func modelArguments(request task.TaskRecord, dialect launchDialect) ([]string, error) {
	if dialect == dialectNativeV2 {
		return variantModelArguments(request.RequestedConfig.Model, request.RequestedConfig.Effort)
	}
	var arguments []string
	if request.RequestedConfig.Model != "" {
		arguments = append(arguments, "--model", request.RequestedConfig.Model)
	}
	if explicitEffort(request.RequestedConfig.Effort) {
		arguments = append(arguments, "--variant", request.RequestedConfig.Effort)
	}
	return arguments, nil
}

// variantModelArguments selects a model variant the only way the current
// dialect accepts one: as the provider/model#variant value of --model.
func variantModelArguments(model, effort string) ([]string, error) {
	if !explicitEffort(effort) {
		if model == "" {
			return nil, nil
		}
		return []string{"--model", model}, nil
	}
	if model == "" {
		return nil, fmt.Errorf("%w: OpenCode effort %q requires an explicit model; OpenCode selects a variant only as --model provider/model#variant", ErrUnsupportedProfile, effort)
	}
	if strings.Contains(model, modelVariantSeparator) {
		return nil, fmt.Errorf("%w: OpenCode model %q already names a variant; omit effort %q or the model's #variant suffix", ErrUnsupportedProfile, model, effort)
	}
	if strings.Contains(effort, modelVariantSeparator) {
		return nil, fmt.Errorf("%w: OpenCode effort %q must not contain %q", ErrUnsupportedProfile, effort, modelVariantSeparator)
	}
	return []string{"--model", model + modelVariantSeparator + effort}, nil
}

func explicitEffort(effort string) bool { return effort != "" && effort != "default" }

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
	if effort := request.RequestedConfig.Effort; explicitEffort(effort) && !validArgumentText(effort) {
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
