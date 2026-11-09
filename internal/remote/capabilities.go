package remote

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

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
