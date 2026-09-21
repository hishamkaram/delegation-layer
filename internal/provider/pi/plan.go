package pi

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

// jsonArguments constructs the direct Pi JSON invocation. The brief is kept
// on the execution core's stdin, which Pi consumes as the initial prompt in
// noninteractive mode. Session continuation is bound to one exact ID rather
// than Pi's "most recent" shorthand.
func jsonArguments(request task.TaskRecord) ([]string, error) {
	return jsonArgumentsForProfile(request, true)
}

func legacyJSONArguments(request task.TaskRecord) ([]string, error) {
	return jsonArgumentsForProfile(request, false)
}

func jsonArgumentsForProfile(request task.TaskRecord, native bool) ([]string, error) {
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	if !native && request.Mode != ModeReadOnly {
		return nil, fmt.Errorf("%w: Pi legacy profile supports read-only only", ErrUnsupportedProfile)
	}
	arguments := []string{"--mode", "json"}
	if request.Mode == ModeReadOnly {
		arguments = append(arguments, "--tools", readOnlyTools)
	}
	if !native {
		// Extensions can replace built-in tools and run lifecycle hooks. A
		// missing configured package can also trigger installation during
		// startup, so offline mode is part of the historical read-only boundary.
		arguments = append(arguments, "--no-extensions", "--offline")
	}
	if request.RequestedConfig.Model != "" {
		arguments = append(arguments, "--model", request.RequestedConfig.Model)
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		arguments = append(arguments, "--thinking", effort)
	}
	if prior := request.PriorSession; prior != nil {
		if prior.Provider != Provider || !validSessionID(prior.ConversationID) || task.ValidateTaskID(prior.PredecessorTaskID) != nil {
			return nil, fmt.Errorf("%w: invalid exact continuation identity", ErrUnsupportedProfile)
		}
		arguments = append(arguments, "--session", prior.ConversationID)
	}
	return arguments, nil
}

// printArguments is kept as the conventional adapter-internal name used by
// the existing provider packages and focused plan tests.
func printArguments(request task.TaskRecord) ([]string, error) {
	return jsonArguments(request)
}

func validateRequest(request task.TaskRecord) error {
	if err := validateMode(request); err != nil {
		return err
	}
	if err := validateOptions(request); err != nil {
		return err
	}
	if err := validateBounds(request); err != nil {
		return err
	}
	if err := task.ValidateTaskID(request.TaskID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return nil
}

func validateMode(request task.TaskRecord) error {
	if request.Provider != Provider || !validMode(request.Mode) || request.RequestedConfig.Permission != request.Mode {
		return fmt.Errorf("%w: Pi requires an explicit supported permission mode", ErrUnsupportedProfile)
	}
	return nil
}

func validateOptions(request task.TaskRecord) error {
	if request.RequestedConfig.NativeTimeout != "" {
		return fmt.Errorf("%w: Pi has no native timeout option", ErrUnsupportedProfile)
	}
	if request.RequestedConfig.Effort != "" && request.RequestedConfig.Effort != "default" && !validThinkingLevel(request.RequestedConfig.Effort) {
		return fmt.Errorf("%w: unsupported Pi thinking level", ErrUnsupportedProfile)
	}
	if strings.ContainsAny(request.RequestedConfig.Model, "\x00\r\n") || strings.TrimSpace(request.RequestedConfig.Model) != request.RequestedConfig.Model {
		return fmt.Errorf("%w: model must be a literal nonblank value", ErrUnsupportedProfile)
	}
	return nil
}

func validateBounds(request task.TaskRecord) error {
	if request.BudgetNanos <= 0 || request.BriefLength <= 0 || request.BriefLength > task.MaxBriefSize {
		return fmt.Errorf("%w: finite brief and positive budget are required", ErrUnsupportedProfile)
	}
	if !filepath.IsAbs(request.CanonicalCwd) || filepath.Clean(request.CanonicalCwd) != request.CanonicalCwd {
		return fmt.Errorf("%w: workspace must be an absolute canonical path", ErrUnsupportedProfile)
	}
	return nil
}

func validThinkingLevel(value string) bool {
	switch value {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}
