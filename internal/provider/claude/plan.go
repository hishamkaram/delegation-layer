package claude

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

const (
	profileSettings  = `{"disableAllHooks":true,"permissions":{"defaultMode":"dontAsk","disableBypassPermissionsMode":"disable"}}` + "\n"
	emptyMCPSettings = `{"mcpServers":{}}` + "\n"
)

// printArguments declares only finite task-owned settings. The shared runner
// materializes their reserved argv slots and delivers the brief over stdin.
func printArguments(request task.TaskRecord) ([]string, []task.InputFile, error) {
	if err := validateRequest(request); err != nil {
		return nil, nil, err
	}
	arguments := []string{
		"--print", "--input-format", "text", "--output-format", "stream-json", "--verbose",
		"--safe-mode", "--restricted", "--tools", "Read,Glob,Grep",
		"--disallowedTools", "mcp__*", "--strict-mcp-config", "--mcp-config", "",
		"--settings", "", "--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--disable-slash-commands", "--no-chrome",
	}
	inputs := []task.InputFile{
		{Name: "empty-mcp.json", ArgumentIndex: 14, Content: emptyMCPSettings},
		{Name: "claude-profile.json", ArgumentIndex: 16, Content: profileSettings},
	}
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
	if request.Provider != Provider || request.Mode != Mode || request.RequestedConfig.Permission != Mode {
		return fmt.Errorf("%w: Claude requires read-only permission", ErrUnsupportedProfile)
	}
	if request.RequestedConfig.Model != "" || (request.RequestedConfig.Effort != "" && request.RequestedConfig.Effort != "default") || request.RequestedConfig.NativeTimeout != "" {
		return fmt.Errorf("%w: only provider-default model and effort are certified", ErrUnsupportedProfile)
	}
	if request.BudgetNanos <= 0 || request.BriefLength <= 0 || request.BriefLength > task.MaxBriefSize {
		return fmt.Errorf("%w: finite brief and positive budget are required", ErrUnsupportedProfile)
	}
	if !filepath.IsAbs(request.CanonicalCwd) || filepath.Clean(request.CanonicalCwd) != request.CanonicalCwd {
		return fmt.Errorf("%w: workspace must be an absolute canonical path", ErrUnsupportedProfile)
	}
	if err := task.ValidateRootID(request.RootID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	if err := task.ValidateTaskID(request.TaskID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return nil
}
