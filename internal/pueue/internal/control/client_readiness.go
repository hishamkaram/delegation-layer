package control

import (
	"context"
	"errors"
)

// Ready verifies that the configured daemon accepts a bounded status request
// with the queue schema required by this client. An empty queue is healthy.
func (c *Client) Ready(ctx context.Context) error {
	result, err := c.readyCommand(ctx)
	if err != nil {
		return bindingProbeError(err)
	}
	return c.validateReadyResult(result)
}

// readyCommand validates the binding immediately before the status probe.
// Version discovery belongs to Bind; unchanged executable bytes do not need a
// second version subprocess before the actual status command.
func (c *Client) readyCommand(ctx context.Context) (CommandResult, error) {
	return c.statusAfterVerifiedVersion(ctx)
}

func (c *Client) statusAfterVerifiedVersion(ctx context.Context) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	if err := c.verifyBinding(); err != nil {
		return CommandResult{}, err
	}
	result, err := c.command(ctx, nil, "status", "--json")
	return classifyReadyStatus(result, err)
}

// groupAfterVerifiedBinding observes only Pueue's bounded group state after
// verifying the saved executable and configuration binding. Private supervisor
// lifecycle controls use this instead of a queue-wide status response when
// they need liveness and ordinary-group scheduling facts, not task rows.
func (c *Client) groupAfterVerifiedBinding(ctx context.Context) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	if err := c.verifyBinding(); err != nil {
		return CommandResult{}, err
	}
	return c.command(ctx, nil, "group", "--json")
}

func classifyReadyStatus(result CommandResult, err error) (CommandResult, error) {
	if errors.Is(err, ErrControlLimit) {
		return result, errors.Join(err, errStatusControlLimit)
	}
	if _, hasSnapshot := result.Snapshot(); hasSnapshot && err != nil {
		return result, errors.Join(ErrBinding, err)
	}
	return result, err
}

func (c *Client) validateReadyResult(result CommandResult) error {
	if result.Err != nil {
		return errors.Join(ErrBinding, result.Err)
	}
	if _, err := snapshotFromResult(result, c.runtimeBinding.ObservedVersion); err != nil {
		return errors.Join(ErrBinding, err)
	}
	return nil
}
