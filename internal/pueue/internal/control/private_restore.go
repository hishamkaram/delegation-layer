package control

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var (
	ErrPrivateRestorePartial = errors.New("private supervisor restore has a partial file")
	ErrPrivateRestoreLayout  = errors.New("private supervisor restore layout is unavailable or changed")
	ErrPrivateRestoreCorrupt = errors.New("private supervisor restore bytes are invalid")
)

type PrivateRestorePresence uint8

const (
	PrivateRestoreMissing PrivateRestorePresence = iota + 1
	PrivateRestorePresent
)

// PrivateRestoreSource identifies the selected native file, not a daemon or
// permission to act. Missing observations have the zero source.
type PrivateRestoreSource struct {
	device, inode, links uint64
	size, modified       int64
	mode                 uint32
}

// PrivateRestoreObservation is a read-only snapshot. Pueue owns the source
// bytes; callers own closure policy and must not infer safety from absence.
type PrivateRestoreObservation struct {
	presence PrivateRestorePresence
	source   PrivateRestoreSource
	snapshot QueueSnapshot
}

func (o PrivateRestoreObservation) Presence() PrivateRestorePresence { return o.presence }
func (o PrivateRestoreObservation) Source() PrivateRestoreSource     { return o.source }
func (o PrivateRestoreObservation) Snapshot() QueueSnapshot          { return cloneQueueSnapshot(o.snapshot) }

// ObservePrivateRestore reads only the config-selected native state for the
// exact published generation. It takes no locks and creates or repairs nothing.
func ObservePrivateRestore(ctx context.Context, root taskdir.RootPin, expectedGeneration task.SupervisorRef, resolution ResolutionContext) (observation PrivateRestoreObservation, resultErr error) {
	if ctx == nil {
		return observation, os.ErrInvalid
	}
	if err := checkPrivateRestoreGeneration(ctx, root, expectedGeneration); err != nil {
		return observation, err
	}
	selected, err := privateRestoreResolution(expectedGeneration, resolution)
	if err != nil {
		return observation, err
	}
	files, err := openPrivateRestore(ctx, root, expectedGeneration, selected)
	defer func() {
		resultErr = errors.Join(resultErr, files.close())
		if resultErr != nil {
			observation = PrivateRestoreObservation{}
		}
	}()
	if err != nil {
		return observation, privateRestoreFault(ErrPrivateRestoreLayout, err)
	}
	observation.presence = PrivateRestoreMissing
	if files.state != nil {
		observation.snapshot, err = parsePrivateRestoreStream(ctx, files.state, files.compressed, expectedGeneration.ObservedVersion)
		if err != nil {
			return observation, err
		}
		observation.presence, observation.source = PrivateRestorePresent, files.source
	}
	if err = files.check(ctx); err != nil {
		return observation, privateRestoreFault(ErrPrivateRestoreLayout, err)
	}
	if err = checkPrivateRestoreGeneration(ctx, root, expectedGeneration); err != nil {
		return observation, err
	}
	return observation, nil
}

func checkPrivateRestoreGeneration(ctx context.Context, root taskdir.RootPin, expected task.SupervisorRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pin, err := readControlRootPin(ctx, root.Root())
	if err != nil {
		return privateRestoreFault(ErrPrivateRestoreLayout, err)
	}
	if !pin.Equal(root) {
		return errors.Join(ErrPrivateRestoreLayout, task.ErrIdentityMismatch)
	}
	if !IsPrivateConfig(root.Root(), expected.ConfigPath) {
		return ErrBinding
	}
	current, found, err := ReadPrivateSupervisorBinding(root.Root())
	if err != nil {
		return err
	}
	if !found || current != expected {
		return ErrBinding
	}
	return ctx.Err()
}

func privateRestoreResolution(expected task.SupervisorRef, supplied ResolutionContext) (ResolutionContext, error) {
	recorded, present, err := resolutionFromBinding(expected)
	if err != nil {
		return ResolutionContext{}, errors.Join(ErrBinding, err)
	}
	if present {
		if supplied != (ResolutionContext{}) && !sameEnvironmentResolution(recorded, supplied) {
			return ResolutionContext{}, ErrBinding
		}
		return recorded, nil
	}
	if err = validateResolutionContext(supplied); err != nil {
		return ResolutionContext{}, errors.Join(ErrBinding, err)
	}
	return supplied, nil
}
