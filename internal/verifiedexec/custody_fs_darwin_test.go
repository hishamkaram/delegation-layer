//go:build darwin

package verifiedexec

import (
	"bytes"
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

func filesystemFacts(path string) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", err
	}
	name := make([]byte, 0, len(stat.Fstypename))
	for _, c := range stat.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return fmt.Sprintf("type=%s flags=0x%x noexec=%t nosuid=%t", name, stat.Flags,
		stat.Flags&unix.MNT_NOEXEC != 0, stat.Flags&unix.MNT_NOSUID != 0), nil
}

// codeSignatureFacts runs the native verifier on the file as launched.
func codeSignatureFacts(path string) string {
	output, err := exec.Command("codesign", "--verify", "--verbose=2", path).CombinedOutput()
	return fmt.Sprintf("codesign --verify exit=%v output=%q", err, bytes.TrimSpace(output))
}
