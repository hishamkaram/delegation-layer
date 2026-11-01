//go:build darwin

package lifetime

import "context"

func checkNativeHostPlacement(ctx context.Context) error {
	// There is no historical-ancestry inference here. The caller's explicit
	// marker and independent user launch contract are the available boundary.
	return ctx.Err()
}
