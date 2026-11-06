//go:build linux

package localpeer

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func read(conn *net.UnixConn) (Credentials, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return Credentials{}, err
	}
	var credentials *unix.Ucred
	var credentialErr error
	err = raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err = errors.Join(err, credentialErr); err != nil {
		return Credentials{}, err
	}
	if credentials == nil || credentials.Pid <= 0 {
		return Credentials{}, ErrCredentials
	}
	return nativeCredentials(int(credentials.Pid), credentials.Uid)
}
