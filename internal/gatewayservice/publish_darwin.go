//go:build darwin

package gatewayservice

import "golang.org/x/sys/unix"

func renameNoReplace(directory int, source, target string) error {
	return unix.RenameatxNp(directory, source, directory, target, unix.RENAME_EXCL)
}
