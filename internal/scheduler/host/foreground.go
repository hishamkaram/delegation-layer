package host

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Foreground fences compatibility run from older managed-host receipts. The
// caller retains H, owns its direct runtime E, and calls Finish after releasing E.
type Foreground struct {
	owner   *Lease
	store   *taskdir.Store
	journal *taskdir.ControlDir
	epoch   epochRecord
}

func (l *Lease) AnnounceForeground(ctx context.Context, store *taskdir.Store, buildID string) (result *Foreground, resultErr error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lock == nil || store == nil || l.rootID != store.RootID || l.root != store.Root || buildID == "" {
		return nil, ErrIdentity
	}
	journal, err := store.OpenSchedulerHostContext(ctx, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			resultErr = errors.Join(resultErr, journal.Close())
		}
	}()
	sequence, err := nextSequence(ctx, journal)
	if err != nil {
		return nil, err
	}
	epoch, err := randomid.NewRandomID()
	if err != nil {
		return nil, err
	}
	ref, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return nil, err
	}
	record := epochRecord{Protocol: ProtocolVersion, RootID: store.RootID, Sequence: sequence, Epoch: epoch, BuildID: buildID, Host: identity(ref), UID: os.Geteuid(), Kind: foregroundController}
	if _, err = publishRecord(ctx, journal, epochName(sequence), record); err != nil {
		return nil, err
	}
	return &Foreground{owner: l, store: store, journal: journal, epoch: record}, nil
}

func (f *Foreground) Finish(ctx context.Context) (resultErr error) {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	if f.owner.lock == nil {
		return os.ErrClosed
	}
	execution, err := scheduler.AcquireOwnerLease(ctx, f.store)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, execution.Close()) }()
	finished := foregroundDoneRecord{RootID: f.epoch.RootID, Epoch: f.epoch.Epoch, Host: f.epoch.Host}
	_, err = publishRecord(ctx, f.journal, foregroundDoneName(f.epoch.Sequence), finished)
	return err
}

func (f *Foreground) Close() error { return f.journal.Close() }

func observeForeground(ctx context.Context, query Query, epoch epochRecord) (Status, error) {
	status := Status{State: "foreground", Epoch: epoch.Epoch, Protocol: epoch.Protocol, BuildID: epoch.BuildID, Host: epoch.Host, LastError: "foreground_stop_required"}
	present, err := identityPresent(epoch.Host)
	if err != nil {
		return status, err
	}
	if !present {
		status.State, status.LastError = "no_host_observed", ""
		return status, nil
	}
	data, err := query.Store.ReadSchedulerHostRecordContext(ctx, foregroundDoneName(epoch.Sequence))
	if task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, fs.ErrNotExist) }) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	var record foregroundDoneRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return status, err
	}
	if record.RootID != epoch.RootID || record.Epoch != epoch.Epoch || record.Host != epoch.Host {
		return status, ErrIdentity
	}
	status.State, status.LastError = "no_host_observed", ""
	return status, nil
}
