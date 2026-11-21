//go:build linux

package workspacefiles

import "testing"

func TestDirectoryPagesPreserveNonUTF8NamesAndRestartAfterChanges(t *testing.T) {
	testDirectoryPagesAreLosslessBoundedAndRestartAfterChanges(t, string([]byte{'b', 0xff}))
}
