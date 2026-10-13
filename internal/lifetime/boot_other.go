//go:build !linux && !darwin

package lifetime

// BootID is unavailable on unsupported hosts.
func BootID() (string, error) {
	return "", ErrUnsupported
}
