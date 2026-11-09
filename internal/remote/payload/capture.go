package payload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const remoteCaptureBudget = 5 * time.Minute

type remoteCaptureStart struct {
	Deadline    string                 `json:"deadline,omitempty"`
	CompletedAt string                 `json:"completed_at,omitempty"`
	Provenance  remotefiles.Provenance `json:"provenance"`
	CapturedAt  string                 `json:"captured_at"`
	Count       int                    `json:"count"`
}

// CaptureOutputs is called by the post-closure owner, never by collect.
// The existing lifetime reader authorizes closure; the task-keyed ControlDir
// transaction owns capture serialization and receipts for ordinary tasks too.
func CaptureOutputs(parent context.Context, store *taskdir.Store, td *taskdir.TaskDir) (receipt remotefiles.Receipt, resultErr error) {
	request, _, err := td.PreparedRecords()
	if err != nil {
		return receipt, err
	}
	if request.Remote == nil || len(request.Remote.OutputPaths) == 0 {
		return receipt, nil
	}
	if err = remotefiles.ValidateSelectors(request.Remote.OutputPaths); err != nil {
		return receipt, err
	}
	if _, err = guardian.ReadClosureProof(parent, td); err != nil {
		return receipt, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), remoteCaptureBudget)
	defer cancel()
	settlement, settleCancel := context.WithTimeout(context.WithoutCancel(parent), remoteCaptureBudget+5*time.Second)
	defer settleCancel()
	journal, err := store.OpenRemoteCaptureContext(settlement, request.TaskID, true)
	if err != nil {
		return receipt, err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	resultErr = journal.WithLock(settlement, func(tx *taskdir.ControlTransaction) error {
		return captureRemoteTransaction(ctx, store, request, tx, &receipt)
	})
	return receipt, resultErr
}

func captureRemoteTransaction(ctx context.Context, store *taskdir.Store, request *task.TaskRecord, tx *taskdir.ControlTransaction, receipt *remotefiles.Receipt) (resultErr error) {
	provenance := remotefiles.Provenance{OriginRootID: request.Remote.OriginRootID, RootID: request.RootID, TaskID: request.TaskID, InputSHA256: request.Remote.InputSHA256}
	began := time.Now().UTC()
	start := remoteCaptureStart{Provenance: provenance, CapturedAt: began.Format(time.RFC3339Nano), Count: len(request.Remote.OutputPaths), Deadline: began.Add(remoteCaptureBudget).Format(time.RFC3339Nano)}
	var complete remoteCaptureStart
	if err := tx.Read("complete.json", &complete); err == nil {
		if complete.Provenance != provenance || complete.Count != start.Count {
			return task.ErrIdentityMismatch
		}
		return loadCompletedRemoteCapture(tx, request, complete, receipt)
	} else if !task.RecordAbsent(err) {
		return err
	}
	var saved remoteCaptureStart
	if err := tx.Read("started.json", &saved); err == nil {
		if saved.Provenance != provenance || saved.Count != start.Count {
			return task.ErrIdentityMismatch
		}
		start = saved
	} else if !task.RecordAbsent(err) {
		return err
	} else if _, err = tx.Put("started.json", start); err != nil {
		return err
	}
	deadline, deadlineErr := remoteCaptureDeadline(start)
	if deadlineErr != nil {
		return deadlineErr
	}
	captureCtx, captureCancel := context.WithDeadline(ctx, deadline)
	defer captureCancel()
	session := remoteCaptureSession{ctx: captureCtx, store: store, request: request, tx: tx, remaining: remotefiles.MaxTotalBytes}
	defer func() { resultErr = errors.Join(resultErr, session.close()) }()
	*receipt = remotefiles.Receipt{Provenance: provenance, CapturedAt: start.CapturedAt, Files: make([]remotefiles.CapturedFile, 0, start.Count)}
	for index, name := range request.Remote.OutputPaths {
		file, err := session.file(index, name, provenance)
		if err != nil {
			return err
		}
		if file.Entry != nil {
			session.remaining -= file.Entry.Size
		}
		receipt.Files = append(receipt.Files, file)
	}
	start.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	receipt.CompletedAt = start.CompletedAt
	_, err := tx.Put("complete.json", start)
	return err
}

func loadCompletedRemoteCapture(tx *taskdir.ControlTransaction, request *task.TaskRecord, complete remoteCaptureStart, receipt *remotefiles.Receipt) error {
	*receipt = remotefiles.Receipt{Provenance: complete.Provenance, CapturedAt: complete.CapturedAt, CompletedAt: complete.CompletedAt, Files: make([]remotefiles.CapturedFile, 0, complete.Count)}
	for index, name := range request.Remote.OutputPaths {
		var file remotefiles.CapturedFile
		if err := tx.Read(fmt.Sprintf("file-%05d.json", index), &file); err != nil {
			return err
		}
		if err := remotefiles.ValidateCapturedFile(file); err != nil {
			return err
		}
		if file.Path != name {
			return task.ErrIdentityMismatch
		}
		receipt.Files = append(receipt.Files, file)
	}
	return nil
}

type remoteCaptureSession struct {
	ctx         context.Context
	store       *taskdir.Store
	request     *task.TaskRecord
	tx          *taskdir.ControlTransaction
	remaining   int64
	source      *os.Root
	destination *os.Root
}

func (s *remoteCaptureSession) close() error {
	var err error
	if s.source != nil {
		err = s.source.Close()
	}
	if s.destination != nil {
		err = errors.Join(err, s.destination.Close())
	}
	return err
}

func (s *remoteCaptureSession) file(index int, name string, provenance remotefiles.Provenance) (remotefiles.CapturedFile, error) {
	recordName := fmt.Sprintf("file-%05d.json", index)
	var file remotefiles.CapturedFile
	if err := s.tx.Read(recordName, &file); err == nil {
		if err = remotefiles.ValidateCapturedFile(file); err != nil {
			return file, err
		}
		if file.Path != name || (file.Entry != nil && file.Entry.Size > s.remaining) {
			return file, task.ErrIdentityMismatch
		}
		return file, nil
	} else if !task.RecordAbsent(err) {
		return file, err
	}
	file = s.capture(name, provenance)
	_, err := s.tx.Put(recordName, file)
	return file, err
}

func (s *remoteCaptureSession) capture(name string, provenance remotefiles.Provenance) remotefiles.CapturedFile {
	file := remotefiles.CapturedFile{Path: name, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if s.ctx.Err() != nil {
		file.Status = "incomplete"
		file.Reason = "capture_deadline"
		return file
	}
	if s.source == nil {
		source, err := os.OpenRoot(s.request.CanonicalCwd)
		if err != nil {
			file.Status = "incomplete"
			file.Reason = "io_error"
			return file
		}
		s.source = source
	}
	if s.destination == nil {
		destination, err := remotedata.Open(s.request.Remote.StorageLayout, s.store.Root, s.request.RootID, s.request.TaskID, remotedata.Outputs, true)
		if err != nil {
			file.Status = "incomplete"
			file.Reason = "destination_unavailable"
			return file
		}
		s.destination = destination
	}
	captured, captureErr := remotefiles.CaptureWithLimit(s.ctx, s.source, s.destination, []string{name}, provenance, s.remaining)
	if captureErr != nil {
		file.Status = "incomplete"
		file.Reason = "io_error"
		return file
	}
	return captured.Files[0]
}

// ReadCapture is passive: it never creates capture state or touches the
// mutable execution workspace. A complete receipt is assembled from bounded
// per-file records so large manifests do not exceed the control-record limit.
func ReadCapture(ctx context.Context, store *taskdir.Store, id string) (receipt remotefiles.Receipt, resultErr error) {
	root, err := os.OpenRoot(store.Root)
	if err != nil {
		return receipt, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	return ReadCapturePassive(ctx, root, store.RootID, id)
}

func openRemoteCaptureDirectory(stateRoot, rootID, taskID string, create bool) (*os.Root, error) {
	return remotedata.Open(task.RemoteDataLegacy, stateRoot, rootID, taskID, remotedata.Outputs, create)
}

func openPrivateRemoteDirectory(parent *os.Root, name string, create bool) (*os.Root, error) {
	return remotedata.OpenPrivate(parent, name, create)
}

func remoteCaptureDeadline(start remoteCaptureStart) (time.Time, error) {
	began, err := time.Parse(time.RFC3339Nano, start.CapturedAt)
	if err != nil {
		return time.Time{}, err
	}
	expected := began.Add(remoteCaptureBudget)
	if start.Deadline == "" {
		return expected, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, start.Deadline)
	if err != nil {
		return time.Time{}, err
	}
	if !deadline.Equal(expected) {
		return time.Time{}, task.ErrEvidenceFault
	}
	return deadline, nil
}
