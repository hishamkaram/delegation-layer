//go:build !linux && !darwin

package lifetime

import "context"

func openPlatformScope(context.Context, Ref) (*Scope, error) {
	return nil, ErrUnsupported
}
