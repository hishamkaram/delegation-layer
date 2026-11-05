package infrastructure

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var (
	ErrOwnerPending      = errors.New("infrastructure owner preparation pending")
	ErrOwnerBusy         = errors.New("infrastructure owner preparation busy")
	ErrOwnerIncompatible = errors.New("infrastructure owner request incompatible")
	ErrOwnerRequest      = errors.New("infrastructure owner request failed")
)

// ProbeOwner authenticates the current resident through its canonical records
// and native socket peer. The caller retains the borrowed reader. Success says
// nothing about private-supervisor readiness or permission to create an owner.
func ProbeOwner(ctx context.Context, store *taskdir.ReadOnlyStore) (StartupReference, error) {
	observed, err := probeOwnerStatus(ctx, store)
	return observed.reference, err
}

// This per-call observation binds later data reads to the authenticated status
// subject. It is neither a cached owner head nor permission to create one.
type ownerStatusObservation struct {
	request   ownerRequest
	pin       taskdir.RootPin
	reference StartupReference
}

func probeOwnerStatus(ctx context.Context, store *taskdir.ReadOnlyStore) (ownerStatusObservation, error) {
	request, pin, err := currentOwnerRequest(ctx, store, ownerStatus)
	if err != nil {
		return ownerStatusObservation{}, err
	}
	reply, err := contactOwner(ctx, store, pin, request)
	if reply == (ownerReply{}) {
		return ownerStatusObservation{}, err
	}
	observed := ownerStatusObservation{request: request, pin: pin, reference: reply.OwnerRef}
	return observed, errors.Join(err, ownerReplyCause(reply.Code))
}

// Selection is a short-lived observation, not another owner head. contactOwner
// rereads the same subject before sending and after receiving its bound reply.
// A finite caller deadline owns this contact independently of startup expiry.
func currentOwnerRequest(ctx context.Context, store *taskdir.ReadOnlyStore, action ownerAction) (ownerRequest, taskdir.RootPin, error) {
	return currentOwnerRequestAtStartup(ctx, store, action, StartupReference{})
}

// A bound selection refuses a replacement before reading its owner records.
// Zero keeps the public client's ordinary current-owner selection behavior.
func currentOwnerRequestAtStartup(ctx context.Context, store *taskdir.ReadOnlyStore, action ownerAction, expected StartupReference) (ownerRequest, taskdir.RootPin, error) {
	if ctx == nil || store == nil {
		return ownerRequest{}, taskdir.RootPin{}, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return ownerRequest{}, taskdir.RootPin{}, err
	}
	deadline, finite := ctx.Deadline()
	if !finite {
		return ownerRequest{}, taskdir.RootPin{}, os.ErrInvalid
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return ownerRequest{}, pin, err
	}
	intent, present, err := readLatestIntent(ctx, store)
	if err != nil {
		return ownerRequest{}, pin, err
	}
	if !present {
		return ownerRequest{}, pin, os.ErrNotExist
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return ownerRequest{}, pin, err
	}
	if err = validateCurrentIntent(intent, pin, boot); err != nil {
		return ownerRequest{}, pin, err
	}
	if expected != (StartupReference{}) {
		reference, referenceErr := ReferenceStartup(intent)
		if referenceErr != nil {
			return ownerRequest{}, pin, referenceErr
		}
		if reference != expected {
			return ownerRequest{}, pin, task.ErrLockBusy
		}
	}
	return requestForCurrentOwner(ctx, store, pin, intent, action, deadline)
}

func requestForCurrentOwner(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, intent StartupIntent, action ownerAction, deadline time.Time) (ownerRequest, taskdir.RootPin, error) {
	reference, err := ReferenceStartup(intent)
	if err != nil {
		return ownerRequest{}, pin, err
	}
	assumption, err := readStartupAssumption(ctx, store, reference)
	if err != nil {
		return ownerRequest{}, pin, err
	}
	publication, err := readOwnerPublication(ctx, store, reference)
	if err != nil {
		return ownerRequest{}, pin, err
	}
	digest, err := publication.Digest(intent, assumption)
	if err != nil {
		return ownerRequest{}, pin, err
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return ownerRequest{}, pin, err
	}
	request := ownerRequest{
		Protocol: ownerProtocolVersion, RootID: pin.RootID(), Owner: reference,
		PublicationSHA256: digest, Client: ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()},
		Action: action, Deadline: deadline.UTC().Format(time.RFC3339Nano),
	}
	return request, pin, nil
}

func ownerReplyCause(code ownerReplyCode) error {
	switch code {
	case ownerOK:
		return nil
	case ownerPending:
		return ErrOwnerPending
	case ownerBusy:
		return ErrOwnerBusy
	case ownerIncompatible:
		return ErrOwnerIncompatible
	case ownerTimeout:
		return errors.Join(ErrOwnerRequest, context.DeadlineExceeded)
	case ownerRefused, ownerFault:
		return ErrOwnerRequest
	}
	return ErrIdentity
}
