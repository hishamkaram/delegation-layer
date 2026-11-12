//go:build !linux && !darwin

package control

import (
	"context"
	"errors"
)

// errPrivateEndpointPeerUnsupported keeps the execution target unknown where
// endpoint peer credentials have no natively verified primitive.
var errPrivateEndpointPeerUnsupported = errors.New("private supervisor endpoint peer identity is unsupported on this platform")

func observeUnixEndpointPeer(ctx context.Context, _ string) (privateEndpointPeer, error) {
	if ctx == nil {
		return privateEndpointPeer{}, context.Canceled
	}
	return privateEndpointPeer{}, errPrivateEndpointPeerUnsupported
}
