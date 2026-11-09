package payload

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// retirementSession borrows the owner store and reader. Its exclusive byte
// lease keeps immutable consumer dependencies stable through proof and deletion.
// Durable state belongs only to its existing owner journal.
type retirementSession struct {
	store   *taskdir.Store
	reader  *taskdir.ReadOnlyStore
	journal *taskdir.ControlDir
	id      string
	role    remotedata.Role
	layout  task.RemoteDataLayout
}

func Retire(ctx context.Context, store *taskdir.Store, id string, role remotedata.Role, apply bool) (report RetirementReport, resultErr error) {
	reader, err := store.RemoteReader(ctx, store.Root)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if !apply {
		return PreviewRetirement(ctx, reader, id, role)
	}
	report = RetirementReport{Role: role, Blockers: []string{}, Families: families(role)}
	if role != remotedata.Origin && role != remotedata.Destination {
		return report, task.ErrEvidenceFault
	}
	if role == remotedata.Origin {
		without, e := originWithoutSnapshot(ctx, reader, id)
		if e != nil {
			return report, e
		}
		if without {
			report.Families = []remotedata.Family{}
			report.Retirable = true
			report.Applied = true
			report.AlreadyDone = true
			return report, nil
		}
	}
	layout, err := CurrentLayout(ctx, reader, id, role)
	if err != nil {
		return report, err
	}
	report.LayoutVersion = layoutVersion(layout)
	lease, err := store.AcquireRemoteByteUse(ctx, id, role == remotedata.Origin, true)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	journal, err := openJournal(ctx, store, id, role, true)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	session := retirementSession{store: store, reader: reader, journal: journal, id: id, role: role, layout: layout}
	record, err := session.tombstone(ctx, &report)
	if err != nil {
		return report, err
	}
	report.Retirable = true
	report.AlreadyDone, err = session.finish(ctx, record)
	report.Applied = err == nil
	return report, err
}

func openJournal(ctx context.Context, store *taskdir.Store, id string, role remotedata.Role, create bool) (*taskdir.ControlDir, error) {
	if role == remotedata.Origin {
		return store.OpenRemoteReferenceContext(ctx, id, create)
	}
	return store.OpenRemoteInputJournalContext(ctx, id, create)
}

func (s retirementSession) tombstone(ctx context.Context, report *RetirementReport) (record RetirementRecord, err error) {
	err = s.journal.Read("retirement.json", &record)
	if err == nil {
		if err = validateRetirement(record, s.store.RootID, s.id, s.role); err != nil {
			return record, err
		}
		if record.StorageLayout != s.layout {
			return record, task.ErrIdentityMismatch
		}
		return record, nil
	}
	if !task.RecordAbsent(err) {
		return record, err
	}
	report.Blockers, err = retirementBlockers(ctx, s.reader, s.id, s.role, s.layout, true)
	if err != nil {
		return record, err
	}
	if len(report.Blockers) > 0 {
		return record, ErrRetirementBlocked
	}
	record = RetirementRecord{Version: 1, RootID: s.store.RootID, TaskID: s.id, Role: s.role, StorageLayout: s.layout, Families: families(s.role), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	err = s.journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error { return s.publishTombstone(ctx, tx, record) })
	return record, err
}

func (s retirementSession) publishTombstone(ctx context.Context, tx *taskdir.ControlTransaction, record RetirementRecord) error {
	if s.role == remotedata.Origin {
		_, err := tx.Put("retirement.json", record)
		return err
	}
	capture, err := s.store.OpenRemoteCaptureContext(ctx, s.id, true)
	if err != nil {
		return err
	}
	err = capture.WithLock(ctx, func(_ *taskdir.ControlTransaction) error { _, e := tx.Put("retirement.json", record); return e })
	return errors.Join(err, capture.Close())
}

func (s retirementSession) finish(ctx context.Context, record RetirementRecord) (bool, error) {
	var complete RetirementRecord
	err := s.journal.Read("retirement-complete.json", &complete)
	if err == nil {
		err = s.journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error { return tx.Verify("retirement-complete.json", record) })
		return err == nil, err
	}
	if !task.RecordAbsent(err) {
		return false, err
	}
	for _, family := range record.Families {
		if err = remotedata.Remove(record.StorageLayout, s.store.Root, s.store.RootID, s.id, family); err != nil {
			return false, err
		}
	}
	err = s.journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		_, e := tx.Put("retirement-complete.json", record)
		return e
	})
	return false, err
}
