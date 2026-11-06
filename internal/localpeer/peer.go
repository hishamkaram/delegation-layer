// Package localpeer observes kernel credentials of connected local Unix peers.
// Consumers own authentication against their saved process/root/epoch facts.
package localpeer

import (
	"errors"
	"net"
	"os"
)

// Credentials is a kernel observation, not proof of a saved process incarnation
// or permission to perform an operation. The kernel owns these connection facts.
type Credentials struct {
	PID int
	UID int
}

// ErrCredentials means native facts were absent or outside their valid range.
var ErrCredentials = errors.New("local peer credentials unavailable or invalid")

// Read observes a connected Unix peer without reading environment or protocol
// data. Native descriptor and socket-option errors retain their original causes.
func Read(conn *net.UnixConn) (Credentials, error) {
	if conn == nil {
		return Credentials{}, os.ErrInvalid
	}
	return read(conn)
}

func nativeCredentials(pid int, uid uint32) (Credentials, error) {
	nativeUID := int(uid)
	if pid <= 0 || uid == ^uint32(0) || nativeUID < 0 || uint64(nativeUID) != uint64(uid) {
		return Credentials{}, ErrCredentials
	}
	return Credentials{PID: pid, UID: nativeUID}, nil
}
