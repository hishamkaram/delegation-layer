package control

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func readControlRootPin(ctx context.Context, root string) (pin taskdir.RootPin, resultErr error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return pin, ErrConfiguration
	}
	if err := rejectSymlinkComponents(root); err != nil {
		return pin, err
	}
	// Metadata-only canonical reads: no probe, maintenance lock or task policy
	// is used. The supplied immutable registry is unused by these root reads.
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, predicate.Default())
	if err != nil {
		return pin, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	return reader.PinRootContext(ctx)
}
