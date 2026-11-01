//go:build linux

package host

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func connectionPeer(conn *net.UnixConn) (int, int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var credentials *unix.Ucred
	var credentialErr error
	err = raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err = errors.Join(err, credentialErr); err != nil {
		return 0, 0, err
	}
	if credentials == nil || credentials.Pid <= 0 {
		return 0, 0, ErrIdentity
	}
	return int(credentials.Pid), int(credentials.Uid), nil
}
