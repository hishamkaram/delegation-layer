package infrastructure

import (
	"context"
	"errors"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (s *ownerServer) executeOtherPreparation(ctx context.Context, work acceptedOwnerWork, reply ownerReply) ownerReply {
	established, err := s.prepareOtherRoot(ctx, work)
	reply.Startup, reply.InitialResultFor = established.Owner, established.InitialResultFor
	if err != nil {
		s.failReply(&reply, err)
	}
	return reply
}

// Capture A's authenticated provenance before opening ANY B resource. No A
// guard survives capture. Accepted server work remains registered throughout B
// establishment, publication observation and parent cleanup. Preparation belongs
// to the client's direct, bound B request.
func (s *ownerServer) prepareOtherRoot(ctx context.Context, work acceptedOwnerWork) (established EstablishedOwner, resultErr error) {
	starter, err := s.entry.captureOwnerProducer(ctx)
	if err != nil {
		return established, err
	}
	resources := &otherPreparationResources{}
	defer func() { resultErr = resources.finish(resultErr) }()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, work.request.Target.Root, predicate.Default())
	resources.reader = reader
	if err != nil {
		return established, err
	}
	pin, selected, err := readOtherSelection(ctx, reader, work.request)
	if err != nil {
		return established, err
	}
	target, err := taskdir.OpenStoreWithPredicatesContext(ctx, pin.Root(), predicate.Default())
	resources.target = target
	if err != nil {
		return established, err
	}
	current, err := target.PinRootContext(ctx)
	if err != nil {
		return established, err
	}
	if !pin.Equal(current) {
		return established, ErrIdentity
	}
	deadline, _ := ctx.Deadline() // Authenticated wire requests require one.
	source := capturedEstablishment{starter: starter, prepare: func(operation context.Context, store *taskdir.Store) (Artifacts, error) {
		return s.host.StageTargetArtifacts(operation, work.operation, work.request.Requirements, store)
	}}
	attempt, establishErr := establishCaptured(ctx, target, EstablishmentRequest{Context: selected, Initial: work.request.Initial, Requirements: work.request.Requirements, Deadline: deadline}, source, producerNative{})
	resources.attempt = attempt
	return prepareEstablishedRoot(ctx, attempt, establishErr)
}

// A nonnil attempt retains cleanup authority even alongside a typed contact
// cause. Only the producer's fully closed nil-attempt result selects a live
// published owner without claiming an initial result.
func prepareEstablishedRoot(ctx context.Context, attempt *StartupAttempt, establishErr error) (EstablishedOwner, error) {
	if attempt == nil {
		var contact *OwnerContactRequiredError
		if errors.As(establishErr, &contact) {
			return EstablishedOwner{Owner: contact.Startup}, nil
		}
		return EstablishedOwner{}, establishErr
	}
	reference := attempt.Reference()
	established := EstablishedOwner{Owner: reference, InitialResultFor: reference}
	if establishErr != nil {
		return established, establishErr
	}
	return established, attempt.WaitForContact(ctx)
}

func readOtherSelection(ctx context.Context, reader *taskdir.ReadOnlyStore, request ownerRequest) (taskdir.RootPin, LaunchContext, error) {
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		return pin, LaunchContext{}, err
	}
	if pin.Root() != request.Target.Root || pin.RootID() != request.Target.RootID {
		return pin, LaunchContext{}, ErrIdentity
	}
	selected, err := readLaunchContext(ctx, reader, request.Context)
	if err != nil {
		return pin, LaunchContext{}, err
	}
	if selected.OS != runtime.GOOS {
		return pin, LaunchContext{}, ErrIdentity
	}
	if err = request.Context.ValidateContext(selected, pin.Root(), pin.RootID()); err != nil {
		return pin, LaunchContext{}, err
	}
	return pin, selected.Clone(), nil
}
