package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

// InitializeConnectorStoreContext is create-only. It never adopts an existing
// directory or upgrades a concurrent initializer's winner, and starts no runtime.
// A failed publication retains its directory for explicit operator recovery.
func InitializeConnectorStoreContext(ctx context.Context, path string, registry predicate.Registry, workspace bool) (_ *Store, resultErr error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("state root must be absolute")
	}
	path = filepath.Clean(path)
	if err := createDirectoriesWithAncestorBarrierContext(ctx, filepath.Dir(path), nil); err != nil {
		return nil, err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, err
	}
	if err := barrierDirAndParent(path, nil); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	store, exists, err := openStoreForInitializationContext(ctx, canonical, registry)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	if exists {
		return nil, os.ErrExist
	}
	if err = store.ProbeFilesystemSupportContext(ctx); err != nil {
		return nil, err
	}
	if err = store.publishInitialConnectorRoot(ctx, workspace); err != nil {
		return nil, err
	}
	keep = true
	return store, nil
}

func (s *Store) publishInitialConnectorRoot(ctx context.Context, workspace bool) error {
	rootID, err := randomid.NewRootID()
	if err != nil {
		return err
	}
	record := task.RootRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if _, err = s.prepareConnectorFeaturesContext(ctx, &record); err != nil {
		return err
	}
	addWorkspaceConnectorFeatures(&record, workspace)
	data, err := marshalControlRecord(record)
	if err != nil {
		return err
	}
	committed, cleanupErr, writeErr := s.stageReaderContext(ctx, s.Root, "root.json", bytes.NewReader(data), nil)
	if err = errors.Join(cleanupErr, writeErr); err != nil {
		return err
	}
	if !committed {
		return fmt.Errorf("%w: root initialization not committed", task.ErrUncertainDurability)
	}
	s.RootID = rootID
	if err = s.RequireConnectorEnabled(ctx); err != nil {
		return err
	}
	return nil
}
