//go:build !linux && !darwin

package lifetime

func openPlatformScope(Ref) (*Scope, error) {
	return nil, ErrUnsupported
}
