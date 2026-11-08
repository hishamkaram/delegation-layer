package remotecommands

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"github.com/hishamkaram/delegation-layer/internal/sshtransport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func enrollRemoteDevice(ctx context.Context, connection sshconfig.Connection, options sshtransport.Options, fingerprint string, c Client, root string) (resultErr error) {
	store, err := c.Stores.Open(ctx, root, true, c.pin)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	journal, err := store.OpenRemoteTrustContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	options.WithTrustLock = func(parent context.Context, operation func() error) error {
		return journal.WithLock(parent, func(_ *taskdir.ControlTransaction) error { return operation() })
	}
	return retryRemoteEnrollment(ctx, remote.RetryPolicy{}, func(attempt context.Context) error {
		return sshtransport.Enroll(attempt, connection, options, fingerprint)
	})
}

// Enrollment retries only transient failures before a host key was accepted.
// SSH verification and the create-once trust commit keep their existing owner.
func retryRemoteEnrollment(ctx context.Context, policy remote.RetryPolicy, enroll func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		bounded, cancel := context.WithTimeout(ctx, remote.DefaultOperationTimeout)
		err := enroll(bounded)
		cancel()
		if err == nil || attempt == remote.MaxAttempts || !retryableEnrollmentFailure(err) {
			return err
		}
		if err = policy.Wait(ctx, attempt); err != nil {
			return err
		}
	}
}

func retryableEnrollmentFailure(err error) bool {
	var transport *sshtransport.Error
	if !errors.As(err, &transport) {
		return false
	}
	return (transport.Code == "unreachable" || transport.Code == "host_key_enrollment_failed") && remote.Transient(err)
}

func listRemoteReferences(ctx context.Context, root, after string, c Client) (report Report, resultErr error) {
	reader, err := c.Stores.Read(ctx, root, c.pin)
	if errors.Is(err, os.ErrNotExist) {
		report.References = []string{}
		return report, nil
	}
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	ids, next, err := reader.RemoteReferenceIDs(ctx, after)
	if err != nil {
		return report, err
	}
	report.References = make([]string, 0, len(ids))
	for _, id := range ids {
		report.References = append(report.References, remote.Handle(id))
	}
	report.NextCursor = next
	return report, nil
}

func remoteCancellationID(ctx context.Context, store *taskdir.Store, id string) (requestID string, resultErr error) {
	journal, err := store.OpenRemoteReferenceContext(ctx, id, false)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	resultErr = journal.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var control remote.Control
		err := tx.Read("cancel-intent.json", &control)
		if err == nil {
			requestID = control.ID
			return task.ValidateTaskID(requestID)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		requestID, err = randomid.NewTaskID()
		if err != nil {
			return err
		}
		_, err = tx.Put("cancel-intent.json", remote.Control{ID: requestID})
		return err
	})
	return requestID, resultErr
}
