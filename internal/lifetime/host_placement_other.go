//go:build !linux && !darwin

package lifetime

import "context"

func checkNativeHostPlacement(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnsupported
}
