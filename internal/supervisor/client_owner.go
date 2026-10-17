package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// supervisorClientOwner is the ownership boundary for every acquired Pueue
// client. Bind may return both a client and an error when a version probe is
// still in flight, so ownership is installed before callers inspect the error.
type ClientOwner struct {
	client *pueue.Client
}

func AcquireClient(client *pueue.Client, err error) (*ClientOwner, error) {
	return &ClientOwner{client: client}, err
}

func (owner *ClientOwner) Client() *pueue.Client {
	if owner == nil {
		return nil
	}
	return owner.client
}

func (owner *ClientOwner) Release() *pueue.Client {
	if owner == nil {
		return nil
	}
	client := owner.client
	owner.client = nil
	return client
}

func (owner *ClientOwner) CloseAfterPending(cause error) error {
	client := owner.Release()
	if client == nil {
		return nil
	}
	return client.CloseAfterPending(cause)
}
