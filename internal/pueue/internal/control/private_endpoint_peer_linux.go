//go:build linux

package control

import (
	"context"
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// observeUnixEndpointPeer connects to the exact Unix endpoint and reads the
// SO_PEERCRED credentials of the process that accepted it. It sends no data.
func observeUnixEndpointPeer(ctx context.Context, path string) (privateEndpointPeer, error) {
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
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return privateEndpointPeer{}, fmt.Errorf("%w: access private supervisor endpoint: %w", ErrBinding, err)
	}
	var credentials *unix.Ucred
	var credentialErr error
	controlErr := raw.Control(func(descriptor uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(descriptor), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err = errors.Join(controlErr, credentialErr); err != nil {
		return privateEndpointPeer{}, fmt.Errorf("%w: read private supervisor endpoint peer: %w", ErrBinding, err)
	}
	if credentials == nil || credentials.Pid <= 0 {
		return privateEndpointPeer{}, fmt.Errorf("%w: private supervisor endpoint peer is unidentified", ErrBinding)
	}
	return privateEndpointPeer{PID: int(credentials.Pid), UID: int(credentials.Uid)}, nil
}
