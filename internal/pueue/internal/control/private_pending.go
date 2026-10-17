package control

import (
	"context"
	"errors"
	"fmt"
)

func reapReadyPending(ctx context.Context, pending *Pending) (CommandResult, error) {
	return awaitPending(ctx, pending)
}

func awaitReadyPending(ctx context.Context, client *Client, pending *Pending) (CommandResult, error) {
	for pending != nil {
		result, contextErr := reapReadyPending(ctx, pending)
		if contextErr != nil && pendingFrom(contextErr) != nil {
			return result, contextErr
		}
		if !pending.VersionProbe() {
			return classifyReadyStatus(result, result.Err)
		}
		if _, versionErr := validateVersionResult(result); versionErr != nil {
			return CommandResult{}, versionErr
		}
		status, statusErr := client.statusAfterVerifiedVersion(ctx)
		if statusErr == nil {
			return status, nil
		}
		if next := pendingFrom(statusErr); next != nil {
			pending = next
			continue
		}
		return status, statusErr
	}
	return CommandResult{}, nil
}

func classifyReadyProcess(result CommandResult) (error, bool) {
	if result.Err == nil {
		return nil, false
	}
	err := errors.Join(ErrBinding, result.Err)
	return err, result.Started && result.ExitCode != 0
}

func processReadyPending(ctx context.Context, client *Client, lastErr error, pending *Pending) (error, bool) {
	result, contextErr := awaitReadyPending(ctx, client, pending)
	if contextErr != nil {
		return errors.Join(lastErr, contextErr), true
	}
	if processErr, retry := classifyReadyProcess(result); processErr != nil {
		if retry {
			return processErr, false
		}
		return processErr, true
	}
	return client.validateReadyResult(result), true
}

func privateResolution(options Options) (ResolutionContext, error) {
	if options.resolutionPinned && options.Resolution != nil {
		return *options.Resolution, nil
	}
	current, err := resolutionContext(environmentForCommand(options.Environment))
	if err != nil {
		return ResolutionContext{}, err
	}
	if options.Resolution == nil {
		return current, nil
	}
	resolution := *options.Resolution
	if err := validateResolutionContext(resolution); err != nil {
		return ResolutionContext{}, err
	}
	if !sameEnvironmentResolution(resolution, current) {
		return ResolutionContext{}, fmt.Errorf("%w: resolution does not match private supervisor environment", ErrConfiguration)
	}
	return resolution, nil
}
