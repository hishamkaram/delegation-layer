package antigravity

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

// printArguments constructs the fixed stdin profile. It does not itself grant
// launch authority: executable, policy sources, environment,
// workspace, and state placement must be validated by the profile resolver.
func printArguments(request task.TaskRecord) ([]string, error) {
	if request.Provider != Provider || request.Mode != Mode || request.RequestedConfig.Permission != Mode {
		return nil, fmt.Errorf("%w: antigravity requires workspace-write", ErrUnsupportedProfile)
	}
	if request.RequestedConfig.Model != "" || (request.RequestedConfig.Effort != "" && request.RequestedConfig.Effort != "default") {
		return nil, fmt.Errorf("%w: model or effort combination is unsupported", ErrUnsupportedProfile)
	}
	if request.BudgetNanos <= 0 {
		return nil, fmt.Errorf("%w: print timeout must be positive", ErrUnsupportedProfile)
	}
	if !filepath.IsAbs(request.CanonicalCwd) || filepath.Clean(request.CanonicalCwd) != request.CanonicalCwd {
		return nil, fmt.Errorf("%w: workspace must be an absolute canonical path", ErrUnsupportedProfile)
	}

	timeout, err := printTimeout(request)
	if err != nil {
		return nil, err
	}
	arguments := []string{
		"--sandbox", "--mode", "accept-edits", "--add-dir", request.CanonicalCwd, "--output-format", "json", "--input-format", "text",
		"--disable-slash-commands", "--print-timeout", timeout,
	}
	if request.PriorSession != nil {
		if request.PriorSession.Provider != Provider || !validConversationID(request.PriorSession.ConversationID) {
			return nil, fmt.Errorf("%w: invalid exact continuation identity", ErrUnsupportedProfile)
		}
		arguments = append(arguments, "--conversation", request.PriorSession.ConversationID)
	}
	return arguments, nil
}

func printTimeout(request task.TaskRecord) (string, error) {
	if request.RequestedConfig.NativeTimeout == "" {
		return time.Duration(request.BudgetNanos).String(), nil
	}
	duration, err := time.ParseDuration(request.RequestedConfig.NativeTimeout)
	if err != nil || duration <= 0 || int64(duration) > request.BudgetNanos {
		return "", fmt.Errorf("%w: native timeout must fit within the task budget", ErrUnsupportedProfile)
	}
	return duration.String(), nil
}
