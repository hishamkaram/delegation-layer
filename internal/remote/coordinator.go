package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Exchange opens/revalidates a destination connection for each Call. Implementors
// must verify SSH trust and return its observed identity on every response.
type Exchange interface {
	Call(context.Context, Request) (Response, error)
}

type Coordinator struct {
	Exchange Exchange
	Retry    RetryPolicy
}

// Dispatch writes send-intent before the first admitting request. A lost response
// switches every later attempt to existing-only reconciliation, even on restart.
func (c Coordinator) Dispatch(ctx context.Context, reference *Reference) (Response, error) {
	intent, err := reference.Intent()
	if err != nil {
		return Response{}, err
	}
	digest, err := IntentDigest(intent)
	if err != nil {
		return Response{}, err
	}
	if rejected, rejectionErr := reference.rejectedResponse(intent, digest); !errors.Is(rejectionErr, os.ErrNotExist) {
		return rejected, rejectionErr
	}
	brief, err := reference.Brief()
	if err != nil {
		return Response{}, err
	}
	sent, err := reference.sendRecorded(digest)
	if err != nil {
		// An unreadable marker cannot establish that no earlier send occurred.
		return Response{}, errors.Join(ErrAcceptanceUnknown, err)
	}
	operation := Reconcile
	if !sent {
		operation = Admit
	}
	request := Request{IntentSHA256: digest, Version: ProtocolVersion, Operation: operation, Expected: &intent.Destination, Root: intent.Destination.Root, TaskID: intent.TaskID, Intent: &intent, Data: brief}
	if operation == Admit {
		request.BeforeWrite = func() error {
			first, gateErr := reference.beginSend(ctx, digest)
			if gateErr != nil {
				return gateErr
			}
			if !first {
				return ErrAdmissionAlreadySent
			}
			return nil
		}
	}
	response, err := c.call(ctx, request)
	if response.TaskRecord == "not_created" {
		return response, errors.Join(ErrAdmissionRejected, reference.recordRejection(ctx, digest, response))
	}
	if err != nil {
		return response, admissionUncertainty(reference, digest, err)
	}
	if err = reference.recordAcceptance(ctx, intent, digest, response); err != nil {
		return response, acceptanceReceiptFailure(reference, digest, err)
	}
	return cacheRemoteObservation(ctx, reference, response), nil
}

func (c Coordinator) Reconcile(ctx context.Context, reference *Reference) (Response, error) {
	intent, err := reference.Intent()
	if err != nil {
		return Response{}, err
	}
	digest, err := IntentDigest(intent)
	if err != nil {
		return Response{}, err
	}
	if rejected, rejectionErr := reference.rejectedResponse(intent, digest); !errors.Is(rejectionErr, os.ErrNotExist) {
		return rejected, rejectionErr
	}
	brief, err := reference.Brief()
	if err != nil {
		return Response{}, err
	}
	response, err := c.call(ctx, Request{Version: ProtocolVersion, Operation: Reconcile, IntentSHA256: digest, Expected: &intent.Destination, Root: intent.Destination.Root, TaskID: intent.TaskID, Intent: &intent, Data: brief})
	if response.TaskRecord == "not_created" {
		return response, errors.Join(ErrAdmissionRejected, reference.recordRejection(ctx, digest, response))
	}
	if err != nil {
		return response, admissionUncertainty(reference, digest, err)
	}
	if err = reference.recordAcceptance(ctx, intent, digest, response); err != nil {
		return response, acceptanceReceiptFailure(reference, digest, err)
	}
	return cacheRemoteObservation(ctx, reference, response), nil
}

func (c Coordinator) Observe(ctx context.Context, reference *Reference, operation Operation, after uint64, watchNanos int64) (Response, error) {
	if operation != Status && operation != Collect && operation != Events {
		return Response{}, fmt.Errorf("%w: not an observation", ErrProtocol)
	}
	intent, err := reference.Intent()
	if err != nil {
		return Response{}, err
	}
	digest, err := IntentDigest(intent)
	if err != nil {
		return Response{}, err
	}
	request := Request{IntentSHA256: digest, Version: ProtocolVersion, Operation: operation, Expected: &intent.Destination, Root: intent.Destination.Root, TaskID: intent.TaskID, After: after, WatchNanos: watchNanos}
	response, err := c.call(ctx, request)
	if err != nil {
		return response, err
	}
	return cacheRemoteObservation(ctx, reference, response), nil
}

func (c Coordinator) Control(ctx context.Context, reference *Reference, operation Operation, control Control) (Response, error) {
	if err := reference.SaveControl(ctx, operation, control); err != nil {
		return Response{}, err
	}
	intent, err := reference.Intent()
	if err != nil {
		return Response{}, err
	}
	digest, err := IntentDigest(intent)
	if err != nil {
		return Response{}, err
	}
	response, err := c.call(ctx, Request{IntentSHA256: digest, Version: ProtocolVersion, Operation: operation, Expected: &intent.Destination, Root: intent.Destination.Root, TaskID: intent.TaskID, Control: &control})
	if err != nil {
		return response, err
	}
	return cacheRemoteObservation(ctx, reference, response), nil
}

// recordAcceptance validates the destination's exact acceptance before writing
// its receipt. Conflicting immutable evidence cannot be repaired by another
// reconciliation; preserve the returned acceptance and report the local fault.
func acceptanceReceiptFailure(reference *Reference, digest string, cause error) error {
	if errors.Is(cause, task.ErrEvidenceFault) {
		return fmt.Errorf("persist verified remote acceptance: %w", cause)
	}
	return admissionUncertainty(reference, digest, cause)
}

// The durable before-write marker survives disconnects and process restarts.
// After it exists, an unverified response cannot prove that admission failed.
// Preserve the underlying failure without turning protocol errors into retries.
func admissionUncertainty(reference *Reference, digest string, cause error) error {
	sent, markerErr := reference.sendRecorded(digest)
	if sent || markerErr != nil {
		return errors.Join(ErrAcceptanceUnknown, cause, markerErr)
	}
	return cause
}

// Observation records are a bounded cache, never admission or control authority.
// A cache fault must not erase a verified destination result after it settles.
func cacheRemoteObservation(ctx context.Context, reference *Reference, response Response) Response {
	if err := reference.Observe(ctx, response); err != nil {
		response.Diagnostics = append(response.Diagnostics, "observation_cache_write_failed")
	}
	return response
}

func (c Coordinator) call(ctx context.Context, request Request) (Response, error) {
	if c.Exchange == nil {
		return Response{}, fmt.Errorf("%w: missing exchange", ErrProtocol)
	}
	if err := request.Validate(); err != nil {
		return Response{}, err
	}
	var response Response
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return response, err
		}
		// Admission timeout never grants another admission transmission.
		bounded, cancel := context.WithTimeout(ctx, DefaultOperationTimeout+time.Duration(request.WatchNanos))
		observed, err := c.Exchange.Call(bounded, request)
		cancel()
		var received *VerifiedResponseError
		if errors.As(err, &received) {
			observed.Diagnostics = append(observed.Diagnostics, "connection_cleanup_incomplete")
			err = nil
		}
		if err == nil {
			return validateExchangeResponse(request, observed)
		}
		if errors.Is(err, ErrAdmissionAlreadySent) {
			request.Operation = Reconcile
			request.BeforeWrite = nil
			continue
		}
		if !Transient(err) || attempt == MaxAttempts {
			return response, err
		}
		if request.Operation == Admit && !beforeSend(err) {
			request.Operation = Reconcile
			request.BeforeWrite = nil
		}
		if err = c.Retry.Wait(ctx, attempt); err != nil {
			return response, err
		}
	}
	return response, errors.New("remote retries exhausted")
}

type OperationError struct {
	Code    string
	Message string
}

func (e *OperationError) Error() string { return "remote " + e.Code + ": " + e.Message }

// Call executes a typed preparation or transfer operation with bounded transport retries.
func (c Coordinator) Call(ctx context.Context, request Request) (Response, error) {
	return c.call(ctx, request)
}

func validateExchangeResponse(request Request, response Response) (Response, error) {
	if err := response.Validate(); err != nil {
		return Response{}, err
	}
	if request.Expected != nil {
		if err := request.Expected.Match(response.Identity); err != nil {
			return Response{}, err
		}
	}
	if response.Failure != nil {
		return response, &OperationError{Code: response.Failure.Code, Message: response.Failure.Message}
	}
	return response, nil
}

// Is preserves admission uncertainty through the machine-readable failure code.
func (e *OperationError) Is(target error) bool {
	return target == ErrAcceptanceUnknown && e.Code == "acceptance_unknown"
}
