package remote

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const OutputSilenceCapability = "output-silence-timeout-v1"

var ErrCapabilityUnsupported = errors.New("remote capability unsupported")

// RequireCapability observes endpoint support without promoting readiness to
// admission authority. Passive tools do not require a serving resident.
func (c Coordinator) RequireCapability(ctx context.Context, destination Identity, capability string) error {
	response, err := c.call(ctx, Request{Version: ProtocolVersion, Operation: Hello, Root: destination.Root, Expected: &destination})
	if err != nil {
		var operation *OperationError
		if !errors.As(err, &operation) || operation.Code != "resident_not_ready" {
			return err
		}
	}
	if !slices.Contains(response.Capabilities, capability) {
		return fmt.Errorf("%w: %s", ErrCapabilityUnsupported, capability)
	}
	return nil
}

// NormalizeOutputSilenceConfig resolves effective off before an intent freezes.
// It never edits an existing intent or changes a positive duration's value.
func NormalizeOutputSilenceConfig(config task.TaskConfig) (task.TaskConfig, error) {
	duration, err := task.ParseOutputSilenceTimeout(config.OutputSilenceTimeout)
	if err != nil {
		return config, err
	}
	config.OutputSilenceTimeout = ""
	if duration > 0 {
		config.OutputSilenceTimeout = duration.String()
	}
	return config, nil
}

// PrepareOutputSilenceConfig checks a previously observed capability list.
// Coordinator admission independently rechecks support before its first send.
func PrepareOutputSilenceConfig(config task.TaskConfig, capabilities []string) (task.TaskConfig, error) {
	normalized, err := NormalizeOutputSilenceConfig(config)
	if err != nil {
		return config, err
	}
	if normalized.OutputSilenceTimeout != "" && !slices.Contains(capabilities, OutputSilenceCapability) {
		return config, ErrCapabilityUnsupported
	}
	return normalized, nil
}
