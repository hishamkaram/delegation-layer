package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"github.com/hishamkaram/delegation-layer/internal/sshtransport"
)

type sshExchange struct {
	connection sshconfig.Connection
	options    sshtransport.Options
}

func (e sshExchange) Call(ctx context.Context, request remote.Request) (response remote.Response, resultErr error) {
	client, err := sshtransport.Dial(ctx, e.connection, e.options)
	if err != nil {
		return response, &remote.BeforeSendError{Err: err}
	}
	defer func() { mergeRemoteConnectionClose(&resultErr, client.Close()) }()
	channel, err := client.Open(ctx)
	if err != nil {
		return response, &remote.BeforeSendError{Err: err}
	}
	defer func() { mergeRemoteConnectionClose(&resultErr, channel.Close()) }()
	frame := remote.Framer{Reader: channel, Writer: channel}
	if err = frame.WriteRequest(request); err != nil {
		return response, err
	}
	if response, err = frame.ReadResponse(); err != nil {
		return response, err
	}
	response.Identity.Host = e.connection.HostName
	response.Identity.Port = e.connection.Port
	response.Identity.User = e.connection.User
	response.Identity.HostKey = client.Fingerprint()
	return response, nil
}

func mergeRemoteConnectionClose(primary *error, cleanup error) {
	if cleanup == nil {
		return
	}
	if *primary == nil {
		*primary = &remote.VerifiedResponseError{Err: cleanup}
		return
	}
	var received *remote.VerifiedResponseError
	if errors.As(*primary, &received) {
		*primary = &remote.VerifiedResponseError{Err: errors.Join(received.Err, cleanup)}
		return
	}
	*primary = errors.Join(*primary, cleanup)
}
