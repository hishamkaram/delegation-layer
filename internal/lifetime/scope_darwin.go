//go:build darwin

package lifetime

func openPlatformScope(ref Ref) (*Scope, error) {
	if ref.kind == KindProcessGroup {
		return openProcessGroupScope(ref)
	}
	return nil, ErrUnsupported
}
