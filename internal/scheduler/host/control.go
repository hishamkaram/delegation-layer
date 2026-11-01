package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

type wireRequest struct {
	Protocol int      `json:"protocol"`
	RootID   string   `json:"root_id"`
	Epoch    string   `json:"epoch"`
	Host     Identity `json:"host"`
	Client   Identity `json:"client"`
	BuildID  string   `json:"build_id"`
	Action   string   `json:"action"`
}

type wireResponse struct {
	Status   wireStatus `json:"status"`
	Accepted bool       `json:"accepted"`
	Code     string     `json:"code,omitempty"`
}

type controlRequest struct {
	request wireRequest
	reply   chan wireResponse
	served  <-chan error
}

// Requests carry exact generation/process bindings. A handler never mutates
// intent; the H-owning loop alone publishes before acknowledging a stop.
func serveConnection(ctx context.Context, conn *net.UnixConn, epoch epochRecord, requests chan<- controlRequest) (resultErr error) {
	var served chan error
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
		if served != nil {
			served <- resultErr
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	finished, watchDone := watchConnection(ctx, conn)
	defer func() { close(finished); resultErr = errors.Join(resultErr, <-watchDone) }()
	var request wireRequest
	if err := readFrame(bufio.NewReader(conn), &request); err != nil {
		return err
	}
	if err := authenticateRequest(conn, epoch, request); err != nil {
		return err
	}
	reply := make(chan wireResponse, 1)
	served = make(chan error, 1)
	select {
	case requests <- controlRequest{request: request, reply: reply, served: served}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case response := <-reply:
		return writeFrame(conn, response)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func contact(ctx context.Context, query Query, epoch epochRecord, action string) (response wireResponse, resultErr error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", epoch.Endpoint)
	if err != nil {
		return response, err
	}
	defer func() { resultErr = errors.Join(resultErr, conn.Close()) }()
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return response, ErrIdentity
	}
	if err = authenticateHost(unixConn, epoch); err != nil {
		return response, err
	}
	deadline := time.Now().Add(5 * time.Second)
	if callerDeadline, bounded := ctx.Deadline(); bounded {
		deadline = callerDeadline
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return response, err
	}
	// The owned cancellation watcher is joined before the connection closes.
	finished, watchDone := watchConnection(ctx, conn)
	defer func() { close(finished); resultErr = errors.Join(resultErr, <-watchDone) }()
	client, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return response, err
	}
	request := wireRequest{Protocol: ProtocolVersion, RootID: epoch.RootID, Epoch: epoch.Epoch, Host: epoch.Host, Client: identity(client), BuildID: query.BuildID, Action: action}
	if err = writeFrame(conn, request); err != nil {
		return response, errors.Join(err, ctx.Err())
	}
	var candidate wireResponse
	if err = readFrame(bufio.NewReader(conn), &candidate); err != nil {
		return response, errors.Join(err, ctx.Err())
	}
	if err = validateResponse(candidate.Status, epoch); err != nil {
		return response, err
	}
	response = candidate
	if response.Code != "" {
		return response, responseError(response.Code)
	}
	return response, nil
}

func authenticateRequest(conn *net.UnixConn, epoch epochRecord, request wireRequest) error {
	peer, uid, err := connectionPeer(conn)
	if err != nil || uid != epoch.UID {
		return errors.Join(ErrIdentity, err)
	}
	if request.Protocol != ProtocolVersion || request.RootID != epoch.RootID || request.Epoch != epoch.Epoch || request.Host != epoch.Host || request.Client.PID != peer {
		return ErrIdentity
	}
	present, err := identityPresent(request.Client)
	if err != nil || !present {
		return errors.Join(ErrIdentity, err)
	}
	return nil
}

func authenticateHost(conn *net.UnixConn, epoch epochRecord) error {
	peer, uid, err := connectionPeer(conn)
	if err != nil || uid != os.Geteuid() || peer != epoch.Host.PID {
		return errors.Join(ErrIdentity, err)
	}
	present, err := identityPresent(epoch.Host)
	if err != nil || !present {
		return errors.Join(ErrIdentity, err)
	}
	return nil
}

func validateResponse(status wireStatus, epoch epochRecord) error {
	if status.Epoch != epoch.Epoch || status.Host != epoch.Host || status.BuildID != epoch.BuildID || status.Protocol != epoch.Protocol {
		return ErrIdentity
	}
	return nil
}

func watchConnection(ctx context.Context, conn net.Conn) (chan struct{}, <-chan error) {
	finished := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			done <- conn.SetDeadline(time.Now())
		case <-finished:
			done <- nil
		}
	}()
	return finished, done
}

func responseError(code string) error {
	switch code {
	case "incompatible":
		return ErrIncompatible
	case "identity":
		return ErrIdentity
	default:
		return fmt.Errorf("scheduler host control failed: %s", code)
	}
}
