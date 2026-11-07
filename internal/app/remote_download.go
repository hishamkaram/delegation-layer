package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type remoteArtifact struct {
	Entry      remotefiles.Entry
	File       *os.File
	Capture    *remotefiles.CapturedFile
	Provenance *remotefiles.Provenance
}

type remoteDownloadReport struct {
	Capture      *remotefiles.CapturedFile `json:"capture,omitempty"`
	Provenance   *remotefiles.Provenance   `json:"provenance,omitempty"`
	Descriptor   remote.Descriptor         `json:"descriptor"`
	Availability string                    `json:"availability"`
	Download     string                    `json:"download"`
	LocalPath    string                    `json:"local_path,omitempty"`
	Error        string                    `json:"error,omitempty"`
}

// Download helpers select sealed evidence and never read mutable workspace outputs.
func downloadRemote(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) (result remote.Response) {
	reader, err := openPinnedRemoteReader(ctx, request.Root, deps)
	if err != nil {
		response.Failure = remoteFailure("state_unavailable", err)
		return response
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			result.Diagnostics = append(result.Diagnostics, "download reader close failed: "+closeErr.Error())
		}
	}()
	stored, requestErr := reader.ReadTaskRequest(ctx, request.TaskID)
	if requestErr != nil || stored.Remote == nil || request.IntentSHA256 == "" || stored.Remote.IntentSHA256 != request.IntentSHA256 {
		response.Failure = remoteFailure("identity_mismatch", errors.Join(task.ErrIdentityMismatch, requestErr))
		return response
	}
	evidence, err := reader.ReadTaskEvidence(ctx, request.TaskID)
	if err != nil {
		response.Failure = remoteFailure("evidence_unavailable", err)
		return response
	}
	if request.Transfer.Selector == "payload" {
		return downloadRemotePayload(ctx, reader, request, response, evidence)
	}
	root, err := os.OpenRoot(request.Root)
	if err != nil {
		response.Failure = remoteFailure("state_unavailable", err)
		return response
	}
	artifact, err := openRemoteDownload(ctx, root, request, stored.Remote)
	if err == nil {
		if artifact.File != nil {
			response, err = readRemoteDownload(request, response, artifact.Entry, artifact.File)
			err = errors.Join(err, artifact.File.Close())
		} else if artifact.Capture != nil {
			response.Transfer = &remote.TransferReceipt{Availability: artifact.Capture.Status, Descriptor: remote.Descriptor{Name: artifact.Capture.Path}}
		}
		if response.Transfer != nil {
			response.Transfer.Capture = artifact.Capture
			response.Transfer.Provenance = artifact.Provenance
		}
	}
	err = errors.Join(err, root.Close())
	if err != nil {
		response.Failure = remoteFailure("download_failed", fmt.Errorf("selected artifact %q: %w", request.Transfer.Selector, err))
	}
	return response
}

func downloadRemotePayload(ctx context.Context, reader *taskdir.ReadOnlyStore, request remote.Request, response remote.Response, evidence taskdir.TaskEvidence) remote.Response {
	entry, file, err := openRemotePayload(ctx, reader, request.TaskID, evidence)
	if err == nil {
		response, err = readRemoteDownload(request, response, entry, file)
		err = errors.Join(err, file.Close())
	}
	if err != nil {
		response.Failure = remoteFailure("download_failed", err)
	}
	return response
}

func openRemoteDownload(ctx context.Context, root *os.Root, request remote.Request, binding *task.RemoteBinding) (artifact remoteArtifact, resultErr error) {
	selector := request.Transfer.Selector
	name, ok := strings.CutPrefix(selector, "output:")
	if !ok {
		return artifact, fmt.Errorf("unknown download selector")
	}
	receipt, err := readRemoteCapturePassive(ctx, root, request.Expected.RootID, request.TaskID)
	if err != nil {
		return artifact, err
	}
	if binding == nil || receipt.Provenance.OriginRootID != binding.OriginRootID || receipt.Provenance.InputSHA256 != binding.InputSHA256 {
		return artifact, task.ErrIdentityMismatch
	}
	captured, err := selectedRemoteCapture(receipt, name)
	if err != nil {
		return artifact, err
	}
	artifact.Capture = &captured
	artifact.Provenance = &receipt.Provenance
	if captured.Status != "captured" {
		return artifact, nil
	}
	artifact.Entry = *captured.Entry
	captureRoot, err := openRemoteCaptureDirectory(request.Root, request.Expected.RootID, request.TaskID, false)
	if err != nil {
		return artifact, err
	}
	artifact.File, err = captureRoot.Open(artifact.Entry.Path)
	err = errors.Join(err, captureRoot.Close())
	if err != nil && artifact.File != nil {
		err = errors.Join(err, artifact.File.Close())
		artifact.File = nil
	}
	return artifact, err
}

func readRemoteDownload(request remote.Request, response remote.Response, entry remotefiles.Entry, file *os.File) (remote.Response, error) {
	if request.Transfer.Offset > entry.Size || request.Transfer.Length > remote.MaxDataBytes || request.Transfer.Length > entry.Size-request.Transfer.Offset {
		return response, remote.ErrProtocol
	}
	response.Transfer = &remote.TransferReceipt{Availability: "available", Descriptor: remote.Descriptor{Name: entry.Path, Length: entry.Size, SHA256: entry.SHA256, Executable: entry.Executable}, Offset: request.Transfer.Offset, Complete: request.Transfer.Offset == entry.Size}
	if request.Transfer.Length == 0 {
		return response, nil
	}
	response.Data = make([]byte, int(request.Transfer.Length))
	n, err := file.ReadAt(response.Data, request.Transfer.Offset)
	if err != nil {
		return response, err
	}
	response.Transfer.Length = int64(n)
	response.Transfer.Offset += int64(n)
	response.Transfer.Complete = response.Transfer.Offset == entry.Size
	return response, nil
}

// readRemoteCapturePassive reads existing bounded immutable records without

func readRemoteCapturePassive(ctx context.Context, root *os.Root, rootID, id string) (remotefiles.Receipt, error) {
	var complete remoteCaptureStart
	if err := readRemoteCaptureRecord(root, id, "complete.json", &complete); err != nil {
		return remotefiles.Receipt{}, err
	}
	if complete.Provenance.RootID != rootID || complete.Provenance.TaskID != id || complete.Count < 0 || complete.Count > remotefiles.MaxEntries {
		return remotefiles.Receipt{}, task.ErrEvidenceFault
	}
	receipt := remotefiles.Receipt{Provenance: complete.Provenance, CapturedAt: complete.CapturedAt, CompletedAt: complete.CompletedAt, Files: make([]remotefiles.CapturedFile, 0, complete.Count)}
	for index := 0; index < complete.Count; index++ {
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		var file remotefiles.CapturedFile
		if err := readRemoteCaptureRecord(root, id, fmt.Sprintf("file-%05d.json", index), &file); err != nil {
			return receipt, err
		}
		if err := remotefiles.ValidateCapturedFile(file); err != nil {
			return receipt, err
		}
		receipt.Files = append(receipt.Files, file)
	}
	return receipt, nil
}

func readRemoteCaptureRecord(root *os.Root, id, name string, value any) (resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return err
	}
	file, err := root.Open(filepath.Join("remote-captures", id, name))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	data, err := task.ReadBounded(file, task.MaxControlRecordSize)
	if err != nil {
		return err
	}
	return task.DecodeStrict(data, value)
}

type remoteDownloadOwner struct {
	Path        string                        `json:"path"`
	Destination remote.Identity               `json:"destination"`
	TaskID      string                        `json:"task_id"`
	Native      remotefiles.DirectoryIdentity `json:"native"`
}

// collectRemoteFiles serializes origin downloads through the reference journal.
// Each task owns only out/taskID, pinned to its journal-recorded native identity.
func collectRemoteFiles(ctx context.Context, store *taskdir.Store, coordinator remote.Coordinator, intent remote.Intent, out string) (descriptors []remoteDownloadReport, resultErr error) {
	if !filepath.IsAbs(out) {
		return nil, fmt.Errorf("download destination must be absolute")
	}
	if err := os.Mkdir(out, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(out)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("download destination must be a directory without symlinks")
	}
	parent, err := os.OpenRoot(out)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	journal, err := store.OpenRemoteReferenceContext(ctx, intent.TaskID, false)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	var destination *os.Root
	if err = journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var openErr error
		destination, openErr = openOwnedDownloadDirectory(parent, out, intent, tx)
		return openErr
	}); err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, destination.Close()) }()
	return downloadSelectedFiles(ctx, coordinator, intent, destination, journal)
}

func openOwnedDownloadDirectory(parent *os.Root, out string, intent remote.Intent, tx *taskdir.ControlTransaction) (result *os.Root, resultErr error) {
	canonical, err := filepath.EvalSymlinks(out)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(canonical))
	record := "download-" + hex.EncodeToString(hash[:16]) + ".json"
	var owner remoteDownloadOwner
	readErr := tx.Read(record, &owner)
	if readErr != nil && !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	if readErr != nil {
		if err = reserveDownloadDirectory(parent, canonical, intent, record, tx); err != nil {
			return nil, err
		}
	}
	directory, err := openPrivateRemoteDirectory(parent, intent.TaskID, false)
	if err != nil {
		if readErr == nil && errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("recorded download directory was removed; choose a new --out: %w", err)
		}
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, directory.Close())
		}
	}()
	native, err := remotefiles.ObserveDirectory(directory)
	if err != nil {
		return nil, err
	}
	expected := remoteDownloadOwner{Path: canonical, Destination: intent.Destination, TaskID: intent.TaskID, Native: native}
	if readErr == nil {
		if owner != expected {
			return nil, task.ErrIdentityMismatch
		}
	} else if _, err = tx.Put(record, expected); err != nil {
		return nil, err
	}
	return directory, nil
}

func downloadSelectedFiles(ctx context.Context, coordinator remote.Coordinator, intent remote.Intent, destination *os.Root, journal *taskdir.ControlDir) (descriptors []remoteDownloadReport, resultErr error) {
	var stageRoot *os.Root
	err := journal.WithLock(ctx, func(_ *taskdir.ControlTransaction) error {
		var openErr error
		stageRoot, openErr = openPrivateRemoteDirectory(destination, ".delegate-download", true)
		return openErr
	})
	if err != nil {
		return nil, err
	}
	native, err := remotefiles.ObserveDirectory(destination)
	if err != nil {
		return nil, errors.Join(err, stageRoot.Close())
	}
	local := remoteDownloadLocal{destination: destination, stageRoot: stageRoot, journal: journal, native: native}
	defer func() { resultErr = errors.Join(resultErr, stageRoot.Close()) }()
	selectors := append([]string{"payload"}, intent.OutputPaths...)
	for index, selector := range selectors {
		prefix := "result/"
		if index > 0 {
			selector = "output:" + selector
			prefix = "files/"
		}
		descriptor, downloadErr := downloadRemoteFile(ctx, coordinator, intent, selector, prefix, local)
		if downloadErr != nil {
			resultErr = errors.Join(resultErr, downloadErr)
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, resultErr
}

func downloadRemoteFile(ctx context.Context, coordinator remote.Coordinator, intent remote.Intent, selector, prefix string, local remoteDownloadLocal) (report remoteDownloadReport, resultErr error) {
	report.Availability = "unknown"
	report.Download = "not_started"
	defer func() {
		if resultErr != nil {
			report.Download = "failed"
			report.Error = resultErr.Error()
		}
	}()
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		return report, err
	}
	request := remote.Request{Version: remote.ProtocolVersion, IntentSHA256: digest, Operation: remote.Download, Root: intent.Destination.Root, Expected: &intent.Destination, TaskID: intent.TaskID, Transfer: &remote.TransferRequest{Selector: selector}}
	response, err := coordinator.Call(ctx, request)
	if err != nil {
		return report, err
	}
	if response.Transfer == nil {
		return report, remote.ErrProtocol
	}
	descriptor := response.Transfer.Descriptor
	if name, ok := strings.CutPrefix(selector, "output:"); ok && descriptor.Name != name {
		return report, remote.ErrIdentity
	}
	report.Descriptor = descriptor
	if response.Transfer.Capture != nil {
		if err = remotefiles.ValidateCapturedFile(*response.Transfer.Capture); err != nil {
			return report, err
		}
		report.Capture = response.Transfer.Capture
		report.Provenance = response.Transfer.Provenance
	}
	if response.Transfer.Availability != "available" {
		return remoteUnavailableDownload(report, *response.Transfer)
	}
	report.Availability = "available"
	report.Download = "partial"
	entry := remotefiles.Entry{Path: prefix + descriptor.Name, Size: descriptor.Length, SHA256: descriptor.SHA256, Executable: descriptor.Executable}
	stage, err := remotefiles.OpenStage(local.stageRoot, remotefiles.Manifest{Entries: []remotefiles.Entry{entry}})
	if err != nil {
		return report, err
	}
	if err = fetchRemoteArtifact(ctx, coordinator, request, descriptor, stage, entry, local); err != nil {
		return report, err
	}
	if err = local.run(ctx, func() error {
		if commitErr := stage.Commit(ctx); commitErr != nil {
			return commitErr
		}
		return stage.Materialize(ctx, local.destination)
	}); err != nil {
		return report, err
	}
	report.Download = "complete"
	report.LocalPath = filepath.Join(local.destination.Name(), entry.Path)
	return report, nil
}

func openRemotePayload(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, evidence taskdir.TaskEvidence) (remotefiles.Entry, *os.File, error) {
	if evidence.Inspection == nil || evidence.Inspection.Outcome == nil {
		return remotefiles.Entry{}, nil, fmt.Errorf("sealed payload unavailable")
	}
	descriptor := evidence.Inspection.Outcome.Payload
	entry := remotefiles.Entry{Path: descriptor.Basename, Size: descriptor.Length, SHA256: descriptor.SHA256}
	file, err := reader.OpenTaskPayload(ctx, id, descriptor)
	return entry, file, err
}

func fetchRemoteArtifact(ctx context.Context, coordinator remote.Coordinator, request remote.Request, descriptor remote.Descriptor, stage *remotefiles.Stage, entry remotefiles.Entry, local remoteDownloadLocal) error {
	var offset int64
	err := local.run(ctx, func() error { var offsetErr error; offset, offsetErr = stage.Offset(ctx, entry.Path); return offsetErr })
	if err != nil {
		return err
	}
	for offset < entry.Size {
		request.Transfer.Offset = offset
		request.Transfer.Length = min(int64(remote.MaxDataBytes), entry.Size-offset)
		response, err := coordinator.Call(ctx, request)
		if err != nil {
			return err
		}
		if response.Transfer == nil || response.Transfer.Descriptor != descriptor || int64(len(response.Data)) != request.Transfer.Length {
			return remote.ErrProtocol
		}
		if err = local.run(ctx, func() error { return stage.WriteChunk(ctx, entry.Path, offset, response.Data) }); err != nil {
			return err
		}
		offset += int64(len(response.Data))
	}
	return nil
}

// remoteDownloadLocal owns only rooted local byte operations. Network calls
// occur outside its short reference transactions, so maintenance is never held
// while waiting for an SSH operation.
type remoteDownloadLocal struct {
	destination *os.Root
	stageRoot   *os.Root
	journal     *taskdir.ControlDir
	native      remotefiles.DirectoryIdentity
}

func (d remoteDownloadLocal) run(ctx context.Context, operation func() error) error {
	return d.journal.WithLock(ctx, func(_ *taskdir.ControlTransaction) error {
		if err := d.checkVisible(); err != nil {
			return err
		}
		if err := operation(); err != nil {
			return err
		}
		return d.checkVisible()
	})
}

func (d remoteDownloadLocal) checkVisible() error {
	visible, err := os.OpenRoot(d.destination.Name())
	if err != nil {
		return err
	}
	native, observeErr := remotefiles.ObserveDirectory(visible)
	if err = errors.Join(observeErr, visible.Close()); err != nil {
		return err
	}
	if native != d.native {
		return task.ErrIdentityMismatch
	}
	return nil
}

func selectedRemoteCapture(receipt remotefiles.Receipt, name string) (remotefiles.CapturedFile, error) {
	for _, file := range receipt.Files {
		if file.Path == name {
			return file, nil
		}
	}
	return remotefiles.CapturedFile{}, os.ErrNotExist
}

func remoteUnavailableDownload(report remoteDownloadReport, receipt remote.TransferReceipt) (remoteDownloadReport, error) {
	report.Availability = receipt.Availability
	report.Download = "not_requested"
	switch receipt.Availability {
	case "absent":
		return report, nil
	case "rejected", "over_budget", "incomplete":
		if receipt.Capture == nil {
			return report, remote.ErrProtocol
		}
		report.Error = receipt.Capture.Reason
		return report, fmt.Errorf("selected output %q unavailable: %s", receipt.Descriptor.Name, receipt.Capture.Reason)
	default:
		return report, remote.ErrProtocol
	}
}

type remoteDownloadReservation struct {
	Path        string          `json:"path"`
	Destination remote.Identity `json:"destination"`
	TaskID      string          `json:"task_id"`
}

func reserveDownloadDirectory(parent *os.Root, canonical string, intent remote.Intent, record string, tx *taskdir.ControlTransaction) error {
	expected := remoteDownloadReservation{Path: canonical, Destination: intent.Destination, TaskID: intent.TaskID}
	name := "pending-" + record
	var saved remoteDownloadReservation
	err := tx.Read(name, &saved)
	if task.RecordAbsent(err) {
		if _, statErr := parent.Lstat(intent.TaskID); !errors.Is(statErr, os.ErrNotExist) {
			if statErr != nil {
				return statErr
			}
			return fmt.Errorf("download directory must be absent before ownership reservation: %w", os.ErrExist)
		}
		if _, err = tx.Put(name, expected); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if saved != expected {
		return task.ErrIdentityMismatch
	}
	if err = parent.Mkdir(intent.TaskID, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	directory, err := openPrivateRemoteDirectory(parent, intent.TaskID, false)
	if err != nil {
		return err
	}
	return errors.Join(validateEmptyDownloadDirectory(directory), directory.Close())
}

func validateEmptyDownloadDirectory(directory *os.Root) (resultErr error) {
	if err := remotefiles.RequirePrivateDirectory(directory); err != nil {
		return err
	}
	handle, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	entries, err := handle.ReadDir(1)
	if len(entries) != 0 {
		return fmt.Errorf("uncommitted download directory contains unowned data")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
