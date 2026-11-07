//go:build linux || darwin

package lifetime

import (
	"os/signal"
	"syscall"
)

// PrepareRemoteControlProcess makes channel loss affect response delivery, not
// the bounded mutation already owned by this dedicated control process.
func PrepareRemoteControlProcess() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
}
