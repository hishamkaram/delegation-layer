//go:build unix

package execution

import "path/filepath"

// implicitWorkingDirectoryEntries returns the PWD assignment exec.Cmd adds on
// POSIX when it inherits the process environment for a command with a Dir.
// The shared launch decision stays OS-neutral by treating this as a list; a
// host without a PWD convention supplies its own file returning none.
func implicitWorkingDirectoryEntries(directory string) []string {
	return []string{"PWD=" + filepath.Clean(directory)}
}
