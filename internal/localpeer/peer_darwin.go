//go:build darwin

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
	var credentials *unix.Xucred
	var pid int
	var credentialErr, pidErr error
	err = raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err = errors.Join(err, credentialErr, pidErr); err != nil {
		return Credentials{}, err
	}
	if credentials == nil || pid <= 0 {
		return Credentials{}, ErrCredentials
	}
	return nativeCredentials(pid, credentials.Uid)
}
