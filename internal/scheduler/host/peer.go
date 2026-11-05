package host

import (
	"errors"
	"net"

	"github.com/hishamkaram/delegation-layer/internal/localpeer"
)

func connectionPeer(conn *net.UnixConn) (int, int, error) {
	credentials, err := localpeer.Read(conn)
	if errors.Is(err, localpeer.ErrCredentials) {
		return 0, 0, errors.Join(ErrIdentity, err)
	}
	if err != nil {
		return 0, 0, err
	}
	return credentials.PID, credentials.UID, nil
}
