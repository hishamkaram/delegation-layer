package claude

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

const (
	profileSettings               = `{"disableAllHooks":true,"permissions":{"defaultMode":"dontAsk","disableBypassPermissionsMode":"disable"}}` + "\n"
	workspaceWriteProfileSettings = `{"disableAllHooks":true,"permissions":{"defaultMode":"acceptEdits","disableBypassPermissionsMode":"disable"}}` + "\n"
	emptyMCPSettings              = `{"mcpServers":{}}` + "\n"
)

// printArguments declares Claude's native headless print plan. The shared
// runner delivers the brief over stdin and owns the process lifetime.
func printArguments(request task.TaskRecord) ([]string, []task.InputFile, error) {
	if err := validateRequest(request); err != nil {
		return nil, nil, err
	}
	permissionMode := "dontAsk"
	if request.Mode == WorkspaceWriteMode {
		permissionMode = "acceptEdits"
	}
	arguments := []string{
		"--print", "--input-format", "text", "--output-format", "stream-json", "--verbose",
		"--permission-mode", permissionMode, "--permission-prompts", "none",
	}
	return appendSessionArguments(request, arguments, nil)
}

// legacyPrintArguments preserves the adapter-owned restrictions recorded by
// older Claude tasks. New admission uses printArguments above; this helper is
// selected only while reconstructing a historical profile.
func legacyPrintArguments(request task.TaskRecord) ([]string, []task.InputFile, error) {
	if err := validateRequest(request); err != nil {
		return nil, nil, err
	}
	tools := "Read,Glob,Grep"
	permissionMode := "dontAsk"
	settings := profileSettings
	if request.Mode == WorkspaceWriteMode {
		tools = "Read,Edit,Write,Glob,Grep"
		permissionMode = "acceptEdits"
		settings = workspaceWriteProfileSettings
	}
	arguments := []string{
		"--print", "--input-format", "text", "--output-format", "stream-json", "--verbose",
		"--safe-mode", "--restricted", "--tools", tools, "--disallowedTools", "mcp__*",
		"--strict-mcp-config", "--mcp-config", "", "--settings", "", "--permission-mode", permissionMode,
		"--permission-prompts", "none", "--disable-slash-commands", "--no-chrome",
	}
	inputs := []task.InputFile{
		{Name: "claude-profile.json", ArgumentIndex: 16, Content: settings},
		{Name: "empty-mcp.json", ArgumentIndex: 14, Content: emptyMCPSettings},
	}
	return appendSessionArguments(request, arguments, inputs)
}

func appendSessionArguments(request task.TaskRecord, arguments []string, inputs []task.InputFile) ([]string, []task.InputFile, error) {
	if prior := request.PriorSession; prior != nil {
		if prior.Provider != Provider || !validSessionID(prior.ConversationID) || task.ValidateTaskID(prior.PredecessorTaskID) != nil {
			return nil, nil, fmt.Errorf("%w: invalid exact continuation identity", ErrUnsupportedProfile)
		}
		return append(arguments, "--resume", prior.ConversationID), inputs, nil
	}
	sessionID, err := FreshSessionID(request.RootID, request.TaskID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid fresh session identity: %w", ErrUnsupportedProfile, err)
	}
	return append(arguments, "--session-id", sessionID), inputs, nil
}

func validateRequest(request task.TaskRecord) error {
	validators := []func(task.TaskRecord) error{
		validateRequestIdentity,
		validateRequestOptions,
		validateRequestBounds,
		validateRequestWorkspace,
		validateRequestIDs,
	}
	for _, validate := range validators {
		if err := validate(request); err != nil {
			return err
		}
	}
	return nil
}

func validateRequestIdentity(request task.TaskRecord) error {
	if request.Provider != Provider || (request.Mode != Mode && request.Mode != WorkspaceWriteMode) || request.RequestedConfig.Permission != request.Mode {
		return fmt.Errorf("%w: Claude does not support the requested permission mode", ErrUnsupportedProfile)
	}
	return nil
}

func validateRequestOptions(request task.TaskRecord) error {
	if request.RequestedConfig.Model != "" || (request.RequestedConfig.Effort != "" && request.RequestedConfig.Effort != "default") || request.RequestedConfig.NativeTimeout != "" {
		return fmt.Errorf("%w: only provider-default model and effort are supported", ErrUnsupportedProfile)
	}
	return nil
}

func validateRequestBounds(request task.TaskRecord) error {
	if request.BudgetNanos <= 0 || request.BriefLength <= 0 || request.BriefLength > task.MaxBriefSize {
		return fmt.Errorf("%w: finite brief and positive budget are required", ErrUnsupportedProfile)
	}
	return nil
}

func validateRequestWorkspace(request task.TaskRecord) error {
	if !filepath.IsAbs(request.CanonicalCwd) || filepath.Clean(request.CanonicalCwd) != request.CanonicalCwd {
		return fmt.Errorf("%w: workspace must be an absolute canonical path", ErrUnsupportedProfile)
	}
	return nil
}

func validateRequestIDs(request task.TaskRecord) error {
	if err := task.ValidateRootID(request.RootID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	if err := task.ValidateTaskID(request.TaskID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return nil
}
