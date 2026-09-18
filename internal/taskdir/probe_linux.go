//go:build linux

package taskdir

import "errors"

// ErrUnsupportedFilesystem indicates that required storage operations are unavailable.
var ErrUnsupportedFilesystem = errors.New("unsupported filesystem: state root does not support required durable operations")

func checkPlatformFilesystemPolicy(rootPath string) error {
	_ = rootPath
	return nil
}
