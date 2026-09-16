package codex

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

// execArguments reserves the task-owned output path and places exec-only
// options before the resume subcommand. The finite brief is always stdin.
func execArguments(request task.TaskRecord) ([]string, int, error) {
	if err := validateRequest(request); err != nil {
		return nil, 0, err
	}
	arguments := []string{
		"exec", "--json", "--color", "never", "--ignore-user-config", "--ignore-rules", "--strict-config",
		"--sandbox", request.Mode, "-c", `approval_policy="never"`, "-c", `approvals_reviewer="user"`, "-c", "allow_login_shell=false",
		"-c", "features.shell_snapshot=false", "-c", "features.shell_snapshot_v2=false", "-c", "features.apps=false",
		"-c", "features.hooks=false", "-c", "features.plugins=false", "-c", `cli_auth_credentials_store="file"`,
		"--cd", request.CanonicalCwd, "--output-last-message", "",
	}
	outputIndex := len(arguments) - 1
	if prior := request.PriorSession; prior != nil {
		if prior.Provider != Provider || !validThreadID(prior.ConversationID) || task.ValidateTaskID(prior.PredecessorTaskID) != nil {
			return nil, 0, fmt.Errorf("%w: invalid exact continuation identity", ErrUnsupportedProfile)
		}
		arguments = append(arguments, "resume", prior.ConversationID)
	}
	return append(arguments, "-"), outputIndex, nil
}

func validateRequest(request task.TaskRecord) error {
	if request.Provider != Provider || (request.Mode != Mode && request.Mode != WorkspaceWriteMode) || request.RequestedConfig.Permission != request.Mode {
		return fmt.Errorf("%w: Codex does not support the requested permission mode", ErrUnsupportedProfile)
	}
	if request.RequestedConfig.Model != "" || (request.RequestedConfig.Effort != "" && request.RequestedConfig.Effort != "default") || request.RequestedConfig.NativeTimeout != "" {
		return fmt.Errorf("%w: only provider-default model and effort are supported", ErrUnsupportedProfile)
	}
	if request.BudgetNanos <= 0 || request.BriefLength <= 0 || request.BriefLength > task.MaxBriefSize {
		return fmt.Errorf("%w: finite brief and positive budget are required", ErrUnsupportedProfile)
	}
	if !filepath.IsAbs(request.CanonicalCwd) || filepath.Clean(request.CanonicalCwd) != request.CanonicalCwd {
		return fmt.Errorf("%w: workspace must be an absolute canonical path", ErrUnsupportedProfile)
	}
	if err := task.ValidateTaskID(request.TaskID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return nil
}
