package antigravity

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

// printArguments constructs the fixed stdin profile. It does not itself grant
// launch authority: executable, policy sources, environment,
// workspace, and state placement must be validated by the profile resolver.
func printArguments(request task.TaskRecord) ([]string, error) {
	if err := validatePrintRequest(request); err != nil {
		return nil, err
	}

	timeout, err := printTimeout(request)
	if err != nil {
		return nil, err
	}
	arguments := fixedPrintArguments(request, timeout)
	arguments = appendModelArguments(arguments, request)
	return appendConversationArguments(request, arguments)
}

func validatePrintRequest(request task.TaskRecord) error {
	validators := []func(task.TaskRecord) error{
		validatePrintIdentity,
		validatePrintOptions,
		validatePrintBounds,
		validatePrintWorkspace,
	}
	for _, validate := range validators {
		if err := validate(request); err != nil {
			return err
		}
	}
	return nil
}

func validatePrintIdentity(request task.TaskRecord) error {
	if request.Provider != Provider || !validMode(request.Mode) || request.RequestedConfig.Permission != request.Mode {
		return fmt.Errorf("%w: antigravity requires an explicit supported permission mode", ErrUnsupportedProfile)
	}
	return nil
}

func validatePrintOptions(request task.TaskRecord) error {
	if model := request.RequestedConfig.Model; model != "" && !validNativeOptionText(model) {
		return fmt.Errorf("%w: model contains invalid bounded text", ErrUnsupportedProfile)
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" && !validNativeOptionText(effort) {
		return fmt.Errorf("%w: effort contains invalid bounded text", ErrUnsupportedProfile)
	}
	return nil
}

func validatePrintBounds(request task.TaskRecord) error {
	if _, err := request.ExecutionBudget(); err != nil {
		return fmt.Errorf("%w: a valid finite or unlimited task budget is required: %w", ErrUnsupportedProfile, err)
	}
	return nil
}

func validatePrintWorkspace(request task.TaskRecord) error {
	if !filepath.IsAbs(request.CanonicalCwd) || filepath.Clean(request.CanonicalCwd) != request.CanonicalCwd {
		return fmt.Errorf("%w: workspace must be an absolute canonical path", ErrUnsupportedProfile)
	}
	return nil
}

func fixedPrintArguments(request task.TaskRecord, timeout string) []string {
	return []string{
		"--sandbox", "--mode", sandboxMode(request.Mode), "--add-dir", request.CanonicalCwd, "--output-format", "json", "--input-format", "text",
		"--disable-slash-commands", "--print-timeout", timeout,
	}
}

func appendModelArguments(arguments []string, request task.TaskRecord) []string {
	if model := request.RequestedConfig.Model; model != "" {
		arguments = append(arguments, "--model", model)
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		arguments = append(arguments, "--effort", effort)
	}
	return arguments
}

func appendConversationArguments(request task.TaskRecord, arguments []string) ([]string, error) {
	if request.PriorSession != nil {
		if request.PriorSession.Provider != Provider || !validConversationID(request.PriorSession.ConversationID) {
			return nil, fmt.Errorf("%w: invalid exact continuation identity", ErrUnsupportedProfile)
		}
		arguments = append(arguments, "--conversation", request.PriorSession.ConversationID)
	}
	return arguments, nil
}

func validMode(mode string) bool {
	return mode == ModeReadOnly || mode == ModeWorkspaceWrite
}

func sandboxMode(mode string) string {
	if mode == ModeReadOnly {
		return "plan"
	}
	return "accept-edits"
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

// unboundedPrintTimeout is agy's documented --print-timeout value that waits
// until the turn completes (agy 1.5.19 help: "0 waits until the turn
// completes (default 0s)"). An unlimited task budget with no native timeout
// maps to it; the guardian still owns stopping the scope.
const unboundedPrintTimeout = "0s"

// printTimeout maps the task budget to agy's print timeout. A finite budget
// bounds the turn and any native timeout; an unlimited budget has no
// enclosing bound, so an explicit positive native timeout is used as given.
func printTimeout(request task.TaskRecord) (string, error) {
	budget, err := request.ExecutionBudget()
	if err != nil {
		return "", fmt.Errorf("%w: a valid finite or unlimited task budget is required: %w", ErrUnsupportedProfile, err)
	}
	if request.RequestedConfig.NativeTimeout == "" {
		if duration, finite := budget.Finite(); finite {
			return duration.String(), nil
		}
		return unboundedPrintTimeout, nil
	}
	duration, err := time.ParseDuration(request.RequestedConfig.NativeTimeout)
	if err != nil || duration <= 0 || !budget.Encloses(duration) {
		return "", fmt.Errorf("%w: native timeout must fit within the task budget", ErrUnsupportedProfile)
	}
	return duration.String(), nil
}
