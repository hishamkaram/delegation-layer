package payload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Origin snapshot helpers retain declared bytes before recording admission intent.
func UploadInputs(ctx context.Context, coordinator remote.Coordinator, intent remote.Intent, manifest remotefiles.Manifest, source *os.Root) (string, error) {
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		return "", err
	}
	request := remote.Request{IntentSHA256: digest, Version: remote.ProtocolVersion, Operation: remote.StageBegin, Expected: &intent.Destination, Root: intent.Destination.Root, TaskID: intent.TaskID, Manifest: &manifest}
	if _, err = coordinator.Call(ctx, request); err != nil {
		return "", err
	}
	for _, entry := range manifest.Entries {
		if err = uploadRemoteFile(ctx, coordinator, request, entry, source); err != nil {
			return "", err
		}
	}
	request.Manifest = nil
	request.Operation = remote.StageCommit
	request.Transfer = &remote.TransferRequest{Selector: "inputs"}
	response, err := coordinator.Call(ctx, request)
	if err != nil {
		return "", err
	}
	if response.Transfer == nil || !response.Transfer.Complete {
		return "", fmt.Errorf("remote inputs have no complete receipt")
	}
	return response.Transfer.Descriptor.Name, nil
}

func uploadRemoteFile(ctx context.Context, coordinator remote.Coordinator, base remote.Request, entry remotefiles.Entry, source *os.Root) (resultErr error) {
	base.Manifest = nil
	base.Transfer = &remote.TransferRequest{Selector: "input:" + entry.Path}
	response, err := coordinator.Call(ctx, base)
	if err != nil {
		return err
	}
	if response.Transfer == nil || response.Transfer.Offset < 0 || response.Transfer.Offset > entry.Size {
		return remote.ErrProtocol
	}
	file, err := source.OpenFile(entry.Path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	offset := response.Transfer.Offset
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	buffer := make([]byte, remotefiles.MaxChunkBytes)
	for offset < entry.Size {
		n, readErr := file.Read(buffer)
		if n > 0 {
			base.Operation = remote.StageChunk
			base.Transfer.Offset = offset
			base.Transfer.Length = int64(n)
			base.Data = buffer[:n]
			sent, sendErr := coordinator.Call(ctx, base)
			if sendErr != nil {
				return sendErr
			}
			if sent.Transfer == nil || sent.Transfer.Offset != offset+int64(n) {
				return remote.ErrProtocol
			}
			offset += int64(n)
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func PrepareSnapshot(ctx context.Context, store *taskdir.Store, id, sourcePath string, paths []string) (manifest remotefiles.Manifest, snapshot *os.Root, resultErr error) {
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		return manifest, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	journal, err := store.OpenRemoteReferenceContext(ctx, id, true)
	if err != nil {
		return manifest, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	resultErr = journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var start remoteInputStart
		if err := tx.Read("manifest-ready.json", &start); err == nil {
			manifest, err = readRemoteManifest(tx)
			if err != nil {
				return err
			}
		} else if !task.RecordAbsent(err) {
			return err
		} else {
			manifest, err = remotefiles.BuildManifest(ctx, source, paths)
			if err != nil {
				return err
			}
			if err = recordRemoteManifest(tx, &manifest); err != nil {
				return err
			}
		}
		root, openErr := openRemoteSnapshotDirectory(store.Root, store.RootID, id, true)
		if openErr != nil {
			return openErr
		}
		openErr = freezeRemoteSnapshot(ctx, root, source, manifest, tx)
		if openErr == nil {
			snapshot, openErr = root.OpenRoot("workspace")
		}
		return errors.Join(openErr, root.Close())
	})
	if resultErr != nil && snapshot != nil {
		resultErr = errors.Join(resultErr, snapshot.Close())
		snapshot = nil
	}
	return manifest, snapshot, resultErr
}

func freezeRemoteSnapshot(ctx context.Context, root, source *os.Root, manifest remotefiles.Manifest, tx *taskdir.ControlTransaction) (resultErr error) {
	digest, err := remotefiles.ManifestSHA256(manifest)
	if err != nil {
		return err
	}
	var saved remoteInputStart
	if err = tx.Read("frozen.json", &saved); err == nil {
		if saved.SHA256 != digest || saved.Count != len(manifest.Entries) {
			return task.ErrIdentityMismatch
		}
		return nil
	} else if !task.RecordAbsent(err) {
		return err
	}
	stage, err := remotefiles.OpenStage(root, manifest)
	if err != nil {
		return err
	}
	for _, entry := range manifest.Entries {
		if err = freezeRemoteFile(ctx, stage, source, entry); err != nil {
			return err
		}
	}
	if err = stage.Commit(ctx); err != nil {
		return err
	}
	execution, err := openPrivateRemoteDirectory(root, "workspace", true)
	if err != nil {
		return err
	}
	err = errors.Join(stage.Materialize(ctx, execution), execution.Close())
	if err != nil {
		return err
	}
	_, err = tx.Put("frozen.json", remoteInputStart{SHA256: digest, Count: len(manifest.Entries)})
	return err
}

func freezeRemoteFile(ctx context.Context, stage *remotefiles.Stage, source *os.Root, entry remotefiles.Entry) (resultErr error) {
	offset, err := stage.Offset(ctx, entry.Path)
	if err != nil {
		return err
	}
	input, err := source.Open(entry.Path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	buffer := make([]byte, remotefiles.MaxChunkBytes)
	for offset < entry.Size {
		length := min(int64(len(buffer)), entry.Size-offset)
		n, readErr := input.ReadAt(buffer[:length], offset)
		if readErr != nil {
			return readErr
		}
		if err = stage.WriteChunk(ctx, entry.Path, offset, buffer[:n]); err != nil {
			return err
		}
		offset += int64(n)
	}
	return nil
}

// LoadSnapshot validates and opens the retained immutable input bytes.
func LoadSnapshot(ctx context.Context, store *taskdir.Store, id string) (manifest remotefiles.Manifest, snapshot *os.Root, resultErr error) {
	journal, err := store.OpenRemoteReferenceContext(ctx, id, false)
	if err != nil {
		return manifest, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	resultErr = journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		manifest, readErr = readRemoteManifest(tx)
		if readErr != nil {
			return readErr
		}
		var frozen remoteInputStart
		if readErr = tx.Read("frozen.json", &frozen); readErr != nil {
			return readErr
		}
		digest, hashErr := remotefiles.ManifestSHA256(manifest)
		if hashErr != nil {
			return hashErr
		}
		if frozen.SHA256 != digest || frozen.Count != len(manifest.Entries) {
			return task.ErrIdentityMismatch
		}
		root, openErr := openRemoteSnapshotDirectory(store.Root, store.RootID, id, false)
		if openErr != nil {
			return openErr
		}
		snapshot, openErr = root.OpenRoot("workspace")
		return errors.Join(openErr, root.Close())
	})
	if resultErr != nil && snapshot != nil {
		resultErr = errors.Join(resultErr, snapshot.Close())
		snapshot = nil
	}
	return manifest, snapshot, resultErr
}
