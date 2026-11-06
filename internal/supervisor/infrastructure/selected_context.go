package infrastructure

import (
	"context"
	"os"
	"runtime"
)

// resolveSelectedContext is the request handler's sole default-selection edge.
// The epoch body owns defaultReference; the requester publishes any selected
// immutable context in this target journal before sending its reference. The
// journal remains authoritative and retains those records until root disposal.
// This returns detached launch data, never native authority or readiness.
func resolveSelectedContext(ctx context.Context, journal *intentJournal, defaultReference, requested LaunchContextReference) (LaunchContext, LaunchContextReference, error) {
	if ctx == nil || journal == nil {
		return LaunchContext{}, LaunchContextReference{}, os.ErrInvalid
	}
	reference := requested
	if reference == (LaunchContextReference{}) {
		reference = defaultReference
	}
	record, err := journal.readContext(ctx, reference)
	if err != nil {
		return LaunchContext{}, LaunchContextReference{}, err
	}
	if record.OS != runtime.GOOS {
		return LaunchContext{}, LaunchContextReference{}, ErrIdentity
	}
	if err = reference.ValidateContext(record, journal.pin.Root(), journal.pin.RootID()); err != nil {
		return LaunchContext{}, LaunchContextReference{}, err
	}
	return record.Clone(), reference, nil
}
