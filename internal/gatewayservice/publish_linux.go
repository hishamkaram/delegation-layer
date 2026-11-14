//go:build linux

package gatewayservice

import "golang.org/x/sys/unix"

func renameNoReplace(directory int, source, target string) error {
	return unix.Renameat2(directory, source, directory, target, unix.RENAME_NOREPLACE)
}
