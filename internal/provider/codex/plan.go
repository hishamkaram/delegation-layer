package codex

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

// execArguments reserves the task-owned output path and places exec-only
// options before the resume subcommand. The finite brief is always stdin.
func execArguments(request task.TaskRecord) ([]string, int, error) {
	return execArgumentsForProfile(request, true)
}

func legacyExecArguments(request task.TaskRecord) ([]string, int, error) {
	return execArgumentsForProfile(request, false)
}

func execArgumentsForProfile(request task.TaskRecord, native bool) ([]string, int, error) {
	if err := validateRequest(request); err != nil {
		return nil, 0, err
	}
	arguments := []string{"exec", "--json", "--color", "never"}
	if native {
		arguments = append(arguments, "--sandbox", request.Mode, "-c", `approval_policy="never"`)
	} else {
		arguments = append(arguments,
			"--ignore-user-config", "--ignore-rules", "--strict-config",
			"--sandbox", request.Mode, "-c", `approval_policy="never"`, "-c", `approvals_reviewer="user"`, "-c", "allow_login_shell=false",
			"-c", "features.shell_snapshot=false", "-c", "features.shell_snapshot_v2=false", "-c", "features.apps=false",
			"-c", "features.hooks=false", "-c", "features.plugins=false", "-c", `cli_auth_credentials_store="file"`)
	}
	arguments = appendModelArguments(arguments, request)
	arguments = append(arguments, "--cd", request.CanonicalCwd, "--output-last-message", "")
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
	validators := []func(task.TaskRecord) error{
		validateRequestIdentity,
		validateRequestOptions,
		validateRequestBounds,
		validateRequestWorkspace,
		validateRequestTaskID,
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
		return fmt.Errorf("%w: Codex does not support the requested permission mode", ErrUnsupportedProfile)
	}
	return nil
}

func validateRequestOptions(request task.TaskRecord) error {
	if request.RequestedConfig.NativeTimeout != "" {
		return fmt.Errorf("%w: Codex has no native timeout option", ErrUnsupportedProfile)
	}
	if model := request.RequestedConfig.Model; model != "" && !validNativeOptionText(model) {
		return fmt.Errorf("%w: model contains invalid bounded text", ErrUnsupportedProfile)
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" && !validNativeOptionText(effort) {
		return fmt.Errorf("%w: effort contains invalid bounded text", ErrUnsupportedProfile)
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

func validateRequestTaskID(request task.TaskRecord) error {
	if err := task.ValidateTaskID(request.TaskID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return nil
}

func appendModelArguments(arguments []string, request task.TaskRecord) []string {
	if model := request.RequestedConfig.Model; model != "" {
		arguments = append(arguments, "--model", model)
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		arguments = append(arguments, "-c", "model_reasoning_effort="+strconv.Quote(effort))
	}
	return arguments
}

func validNativeOptionText(value string) bool {
	if value == "" || len(value) > task.MaxControlRecordSize || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || character >= 0x80 && character <= 0x9f {
			return false
		}
	}
	return true
}
