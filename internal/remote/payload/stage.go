package payload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type (
	remoteInputStart struct {
		SHA256 string `json:"sha256"`
		Count  int    `json:"count"`
	}
	remoteInputReady struct {
		SHA256 string `json:"sha256"`
		Cwd    string `json:"cwd"`
	}
)

func Stage(ctx context.Context, store *taskdir.Store, request remote.Request, response remote.Response) (remote.Response, error) {
	journal, err := store.OpenRemoteInputJournalContext(ctx, request.TaskID, true)
	if err != nil {
		return response, err
	}
	operationErr := journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := bindRemoteStageOwner(tx, request); err != nil {
			return err
		}
		if request.Operation == remote.StageBegin && request.Transfer.Selector == "manifest" {
			return recordRemoteManifest(tx, request.Manifest)
		}
		manifest, readErr := readRemoteManifest(tx)
		if readErr != nil {
			return readErr
		}
		root, readErr := openRemoteInputDirectory(store.Root, store.RootID, request.TaskID, true)
		if readErr != nil {
			return readErr
		}
		readErr = operateRemoteStage(ctx, store, request, manifest, tx, root, &response)
		return errors.Join(readErr, root.Close())
	})
	return response, errors.Join(operationErr, journal.Close())
}

func recordRemoteManifest(tx *taskdir.ControlTransaction, manifest *remotefiles.Manifest) error {
	if manifest == nil {
		return fmt.Errorf("missing input manifest")
	}
	digest, err := remotefiles.ManifestSHA256(*manifest)
	if err != nil {
		return err
	}
	if _, err = tx.Put("manifest.json", remoteInputStart{SHA256: digest, Count: len(manifest.Entries)}); err != nil {
		return err
	}
	entries := append([]remotefiles.Entry(nil), manifest.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for index, entry := range entries {
		if _, err = tx.Put(fmt.Sprintf("input-%05d.json", index), entry); err != nil {
			return err
		}
	}
	_, err = tx.Put("manifest-ready.json", remoteInputStart{SHA256: digest, Count: len(manifest.Entries)})
	return err
}

func readRemoteManifest(tx *taskdir.ControlTransaction) (remotefiles.Manifest, error) {
	var start remoteInputStart
	if err := tx.Read("manifest-ready.json", &start); err != nil {
		return remotefiles.Manifest{}, err
	}
	if start.Count < 0 || start.Count > remotefiles.MaxEntries {
		return remotefiles.Manifest{}, task.ErrEvidenceFault
	}
	manifest := remotefiles.Manifest{Entries: make([]remotefiles.Entry, 0, start.Count)}
	for index := 0; index < start.Count; index++ {
		var entry remotefiles.Entry
		if err := tx.Read(fmt.Sprintf("input-%05d.json", index), &entry); err != nil {
			return manifest, err
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	digest, err := remotefiles.ManifestSHA256(manifest)
	if err != nil {
		return manifest, err
	}
	if digest != start.SHA256 {
		return manifest, task.ErrIdentityMismatch
	}
	return manifest, nil
}

func operateRemoteStage(ctx context.Context, store *taskdir.Store, request remote.Request, manifest remotefiles.Manifest, tx *taskdir.ControlTransaction, root *os.Root, response *remote.Response) error {
	stage, err := remotefiles.OpenStage(root, manifest)
	if err != nil {
		return err
	}
	selector := request.Transfer.Selector
	if request.Operation == remote.StageCommit && selector == "inputs" {
		return commitRemoteInputs(ctx, store, request.TaskID, manifest, tx, root, stage, response)
	}
	name, ok := strings.CutPrefix(selector, "input:")
	if !ok {
		return fmt.Errorf("unsupported input selector")
	}
	var offset int64
	switch request.Operation {
	case remote.StageBegin:
		offset, err = stage.Offset(ctx, name)
	case remote.StageChunk:
		err = stage.WriteChunk(ctx, name, request.Transfer.Offset, request.Data)
		if err == nil {
			offset, err = stage.Offset(ctx, name)
		}
	case remote.Hello, remote.Prepare, remote.Admit, remote.Reconcile, remote.Status, remote.Collect, remote.Cancel, remote.Events, remote.Reply, remote.Steer, remote.StageCommit, remote.Download:
		return remote.ErrProtocol
	}
	if err == nil {
		response.Transfer = &remote.TransferReceipt{Offset: offset}
	}
	return err
}

func commitRemoteInputs(ctx context.Context, store *taskdir.Store, id string, manifest remotefiles.Manifest, tx *taskdir.ControlTransaction, root *os.Root, stage *remotefiles.Stage, response *remote.Response) error {
	digest, err := remotefiles.ManifestSHA256(manifest)
	if err != nil {
		return err
	}
	expected := remoteInputReady{SHA256: digest, Cwd: InputWorkspace(store.Root, store.RootID, id)}
	var saved remoteInputReady
	if err = tx.Read("ready.json", &saved); err == nil {
		if saved != expected {
			return task.ErrIdentityMismatch
		}
		response.Transfer = &remote.TransferReceipt{Complete: true, Descriptor: remote.Descriptor{Name: saved.Cwd, SHA256: digest}}
		return nil
	} else if !task.RecordAbsent(err) {
		return err
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
	if _, err = tx.Put("ready.json", expected); err != nil {
		return err
	}
	response.Transfer = &remote.TransferReceipt{Complete: true, Descriptor: remote.Descriptor{Name: expected.Cwd, SHA256: digest}}
	return nil
}

func InputWorkspace(root, rootID, id string) string {
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".remote-inputs", rootID, id, "workspace")
}

func openRemoteInputDirectory(stateRoot, rootID, id string, create bool) (*os.Root, error) {
	return openRemoteByteDirectory(stateRoot, rootID, id, "remote-inputs", create)
}

func openRemoteSnapshotDirectory(stateRoot, rootID, id string, create bool) (*os.Root, error) {
	return openRemoteByteDirectory(stateRoot, rootID, id, "remote-snapshots", create)
}

func openRemoteByteDirectory(stateRoot, rootID, id, family string, create bool) (*os.Root, error) {
	if err := task.ValidateRootID(rootID); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	current, err := os.OpenRoot(filepath.Dir(stateRoot))
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"." + filepath.Base(stateRoot) + "." + family, rootID, id} {
		next, openErr := openPrivateRemoteDirectory(current, name, create)
		err = errors.Join(openErr, current.Close())
		if err != nil {
			if next != nil {
				err = errors.Join(err, next.Close())
			}
			return nil, err
		}
		current = next
	}
	return current, nil
}

func VerifyInputs(ctx context.Context, store *taskdir.Store, id, intentSHA, inputSHA, cwd string) (resultErr error) {
	if inputSHA == "" {
		return nil
	}
	journal, err := store.OpenRemoteInputJournalContext(ctx, id, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	var owner remoteStageOwner
	if err = journal.Read("stage-owner.json", &owner); err != nil {
		return err
	}
	if owner.IntentSHA256 != intentSHA {
		return task.ErrIdentityMismatch
	}
	var ready remoteInputReady
	if err = journal.Read("ready.json", &ready); err != nil {
		return err
	}
	if ready.SHA256 != inputSHA || ready.Cwd != cwd || cwd != InputWorkspace(store.Root, store.RootID, id) {
		return task.ErrIdentityMismatch
	}
	return nil
}

type remoteStageOwner struct {
	IntentSHA256 string `json:"intent_sha256"`
}

func bindRemoteStageOwner(tx *taskdir.ControlTransaction, request remote.Request) error {
	if err := task.ValidateSHA256(request.IntentSHA256); err != nil {
		return err
	}
	owner := remoteStageOwner{IntentSHA256: request.IntentSHA256}
	var saved remoteStageOwner
	err := tx.Read("stage-owner.json", &saved)
	if err == nil {
		if saved != owner {
			return task.ErrRequestConflict
		}
		return tx.Verify("stage-owner.json", owner)
	}
	if !task.RecordAbsent(err) {
		return err
	}
	if request.Operation != remote.StageBegin || request.Transfer.Selector != "manifest" {
		return err
	}
	if request.Manifest == nil {
		return remote.ErrProtocol
	}
	if err = remotefiles.ValidateManifest(*request.Manifest); err != nil {
		return err
	}
	// A missing owner cannot authorize adopting previously staged bytes after
	// external deletion of its identity record.
	for _, name := range []string{"manifest.json", "manifest-ready.json", "ready.json"} {
		var previous remoteInputStart
		if readErr := tx.Read(name, &previous); !task.RecordAbsent(readErr) {
			return errors.Join(task.ErrEvidenceFault, readErr)
		}
	}
	_, err = tx.Put("stage-owner.json", owner)
	return err
}
