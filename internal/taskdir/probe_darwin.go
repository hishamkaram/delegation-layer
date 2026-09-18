//go:build darwin

package taskdir

import "errors"

// ErrUnsupportedFilesystem indicates that the storage filesystem policy was violated.
var ErrUnsupportedFilesystem = errors.New("unsupported filesystem: state root does not support required durable operations")

func checkPlatformFilesystemPolicy(rootPath string) error {
	_ = rootPath
	return nil
}
