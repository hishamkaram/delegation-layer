//go:build darwin

package control

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/hishamkaram/delegation-layer/internal/localpeer"
)

// observeUnixEndpointPeer owns only a short-lived connection. localpeer owns
// native credential reads; the caller binds those facts to a process incarnation.
func observeUnixEndpointPeer(ctx context.Context, path string) (privateEndpointPeer, error) {
	if ctx == nil {
		return privateEndpointPeer{}, context.Canceled
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return privateEndpointPeer{}, fmt.Errorf("%w: connect private supervisor endpoint: %w", ErrBinding, err)
	}
	peer, peerErr := unixConnectionPeer(connection)
	if closeErr := connection.Close(); closeErr != nil {
		return privateEndpointPeer{}, errors.Join(peerErr, fmt.Errorf("%w: close private supervisor endpoint: %w", ErrBinding, closeErr))
	}
	return peer, peerErr
}

func unixConnectionPeer(connection net.Conn) (privateEndpointPeer, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return privateEndpointPeer{}, fmt.Errorf("%w: private supervisor endpoint is not a Unix connection", ErrBinding)
	}
	credentials, err := localpeer.Read(unixConnection)
	if err != nil {
		return privateEndpointPeer{}, fmt.Errorf("%w: read private supervisor endpoint peer: %w", ErrBinding, err)
	}
	return privateEndpointPeer{PID: credentials.PID, UID: credentials.UID}, nil
}
