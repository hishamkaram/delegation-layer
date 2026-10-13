//go:build darwin

package lifetime

import "context"

func openPlatformScope(ctx context.Context, ref Ref) (*Scope, error) {
	if ref.kind == KindProcessGroup {
		return openProcessGroupScope(ctx, ref)
	}
	return nil, ErrUnsupported
}
