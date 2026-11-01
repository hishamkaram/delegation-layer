//go:build darwin

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
	var credentials *unix.Xucred
	var pid int
	var credentialErr, pidErr error
	err = raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err = errors.Join(err, credentialErr, pidErr); err != nil {
		return 0, 0, err
	}
	if credentials == nil || pid <= 0 {
		return 0, 0, ErrIdentity
	}
	return pid, int(credentials.Uid), nil
}
