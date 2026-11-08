package remotecommands

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// testStores supplies real durable stores at the command owner's narrow port.
// Native app tests separately exercise the canonical frontend opener and pin.
type testStores struct{}

func (testStores) Open(ctx context.Context, root string, create bool, pin *taskdir.RootPin) (*taskdir.Store, error) {
	var store *taskdir.Store
	var err error
	if create && pin == nil {
		store, err = taskdir.InitStoreWithPredicatesContext(ctx, root, predicate.Default())
	} else {
		store, err = taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
	}
	if err != nil {
		return nil, err
	}
	if pin != nil {
		actual, pinErr := store.PinRootContext(ctx)
		if pinErr == nil && !actual.Equal(*pin) {
			pinErr = remote.ErrIdentity
		}
		if pinErr != nil {
			return nil, errors.Join(pinErr, store.Close())
		}
	}
	return store, nil
}

func (testStores) Read(ctx context.Context, root string, pin *taskdir.RootPin) (*taskdir.ReadOnlyStore, error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, predicate.Default())
	if err != nil {
		return nil, err
	}
	if pin != nil {
		actual, pinErr := reader.PinRootContext(ctx)
		if pinErr == nil && !actual.Equal(*pin) {
			pinErr = remote.ErrIdentity
		}
		if pinErr != nil {
			return nil, errors.Join(pinErr, reader.Close())
		}
	}
	return reader, nil
}

func newTestReference(t *testing.T) (*remote.Reference, *taskdir.Store) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	id := strings.Repeat("a", 32)
	reference, err := remote.OpenReference(t.Context(), store, id, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reference.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("brief")
	intent := remote.Intent{Version: remote.ProtocolVersion, OriginRootID: store.RootID, TaskID: id, Device: "worker", Executable: "delegate", Destination: remote.Identity{Host: "worker.example", Port: 22, User: "runner", HostKey: "SHA256:fixture", Root: store.Root, RootID: strings.Repeat("b", 32)}, Provider: "claude:print", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: cwd, Brief: remote.Descriptor{Name: "brief", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)}}
	if err = reference.SaveIntent(t.Context(), intent, brief); err != nil {
		t.Fatal(err)
	}
	return reference, store
}
