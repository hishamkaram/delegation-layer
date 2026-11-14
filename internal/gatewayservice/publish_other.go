//go:build !linux && !darwin

package gatewayservice

func renameNoReplace(int, string, string) error { return ErrConfiguration }
