package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ReadTaskLogDescriptors observes raw stream availability and expected seal
// metadata. It does not validate captured bytes or terminal publication.
func (r *ReadOnlyStore) ReadTaskLogDescriptors(ctx context.Context, id string) ([]LogDescriptor, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	return td.logObservationDescriptors(ctx)
}

func (td *TaskDir) logObservationDescriptors(ctx context.Context) ([]LogDescriptor, error) {
	_, meta, specHash, metaHash, err := td.loadAndValidatePreparedSetContext(ctx)
	if err != nil {
		return nil, err
	}
	seal, err := td.readSealMetadata(meta.Predicate, specHash, metaHash, func(path string, record any) error {
		return td.store.readRecordContext(ctx, path, record)
	})
	if err != nil && !errors.Is(err, task.ErrNoSeal) {
		return nil, err
	}
	var descriptors []LogDescriptor
	for _, name := range []string{"stderr", "stdout"} {
		descriptor, observeErr := td.observeLogDescriptor(name, seal)
		if observeErr != nil {
			return nil, observeErr
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, ctx.Err()
}

func (td *TaskDir) observeLogDescriptor(name string, seal *task.ProviderExitRecord) (LogDescriptor, error) {
	descriptor := LogDescriptor{Path: filepath.Join(td.Dir, "raw", name)}
	if seal != nil {
		for _, entry := range seal.RawManifest {
			if entry.Path == "raw/"+name {
				size := entry.Size
				descriptor.Sealed, descriptor.Size, descriptor.SHA256 = true, &size, entry.SHA256
				break
			}
		}
	}
	file, err := td.store.openFile(descriptor.Path, os.O_RDONLY)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return descriptor, err
	}
	descriptor.Available = err == nil
	if file != nil {
		return descriptor, file.Close()
	}
	return descriptor, nil
}
