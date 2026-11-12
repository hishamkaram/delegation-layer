package host

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

// Observe only reads pinned records and authenticates the recorded endpoint.
// It never opens or probes H. Absence/refusal is not proof of stopped state.
func Observe(ctx context.Context, query Query) (Status, error) {
	if ctx == nil || query.Store == nil {
		return Status{}, errors.New("scheduler host observation requires an existing passive reader and context")
	}
	epoch, present, err := readLatest(ctx, query.Store, query.Store.RootID())
	if err != nil {
		return Status{State: "degraded", LastError: "host_unreachable"}, err
	}
	if !present {
		return Status{State: "no_host_observed", Protocol: ProtocolVersion}, nil
	}
	if epoch.Kind == foregroundController {
		return observeForeground(ctx, query, epoch)
	}
	response, err := exchange[wireHealthResponse](ctx, query, epoch, "health")
	if err == nil {
		err = validateResponse(response.Status, epoch)
	}
	if err == nil && response.Code != "" && response.Code != "invalid_control" {
		err = responseError(response.Code)
	}
	if err != nil {
		status := Status{State: "degraded", Epoch: epoch.Epoch, Protocol: epoch.Protocol, BuildID: epoch.BuildID, Host: epoch.Host, LastError: "host_unreachable"}
		if noHostConnection(err) {
			status.State, status.LastError = "no_host_observed", ""
			return status, nil
		}
		return status, stopReplyError(err)
	}
	status, err := response.Status.value()
	if err != nil {
		return Status{State: "degraded", LastError: "host_unreachable"}, errors.Join(ErrIdentity, err)
	}
	health, healthErr := installationHealthFromResponse(response)
	return applyInstallationHealth(status, health), healthErr
}

// Stop captures one exact generation and never redirects its request to a
// replacement host. Acceptance follows the owner's durable stopped fence;
// completion additionally observes exact host/child process absence.
func Stop(ctx context.Context, query Query) (StopResult, error) {
	if ctx == nil || query.Store == nil {
		return StopResult{}, errors.New("scheduler host stop requires an existing passive reader and context")
	}
	epoch, present, err := readLatest(ctx, query.Store, query.Store.RootID())
	if err != nil {
		return StopResult{}, err
	}
	if !present {
		return StopResult{Status: Status{State: "no_host_observed", Protocol: ProtocolVersion}}, ErrPending
	}
	if epoch.Kind == foregroundController {
		status, observeErr := observeForeground(ctx, query, epoch)
		return StopResult{Status: status}, errors.Join(ErrPending, observeErr)
	}
	response, requestErr := contact(ctx, query, epoch, "stop")
	return recoverStoppedFence(ctx, query, epoch, response, requestErr)
}

func awaitIdentityAbsent(ctx context.Context, value Identity) error {
	ref, err := lifetime.NewProcessRef(value.PID, value.StartToken)
	if err != nil {
		return err
	}
	return lifetime.WaitProcessAbsent(ctx, ref)
}
