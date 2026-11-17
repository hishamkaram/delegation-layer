package resultread

import "golang.org/x/sys/unix"

// Search-only handles preserve native identity without requiring directory reads.
const directorySearchFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
