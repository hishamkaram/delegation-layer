//go:build linux

package workspacefiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func changeTime(stat *syscall.Stat_t) string {
	return strconv.FormatInt(stat.Ctim.Sec, 10) + ":" + strconv.FormatInt(stat.Ctim.Nsec, 10)
}

// O_PATH acquires the inode without opening a FIFO/device for content. Reopen
// the held regular inode through this process's descriptor, so replacement of
// the selected pathname cannot open a device between stat and content open.
func openContent(root *os.Root, path string) (_ *os.File, resultErr error) {
	return openHeldContent(root, path, 0)
}

func openHeldContent(root *os.Root, path string, followed int) (content *os.File, resultErr error) {
	if followed > 8 {
		return nil, failure(Changed, "file_link", syscall.ELOOP)
	}
	held, err := root.OpenFile(path, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeErr := classify("file_close", held.Close())
		if closeErr != nil && content != nil {
			closeErr = errors.Join(closeErr, classify("file_close", content.Close()))
			content = nil
		}
		resultErr = errors.Join(resultErr, closeErr)
	}()
	info, err := held.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// os.Root uses O_NOFOLLOW internally; O_PATH succeeds on that final
		// symlink rather than returning ELOOP. Resolve its held target explicitly
		// and let Root enforce containment again. Keep '..' after a symlinked
		// parent uncleaned so native traversal retains its actual meaning.
		buffer := make([]byte, 4097)
		n, readErr := unix.Readlinkat(int(held.Fd()), "", buffer)
		if readErr != nil {
			return nil, readErr
		}
		target := string(buffer[:n])
		if n > 4096 || filepath.IsAbs(target) {
			return nil, failure(OutsideScope, "file_link", nil)
		}
		prefix := ""
		if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
			prefix = path[:slash+1]
		}
		return openHeldContent(root, prefix+target, followed+1)
	}
	if !info.Mode().IsRegular() {
		return nil, failure(Changed, "file_kind", nil)
	}
	file, err := os.OpenFile("/proc/self/fd/"+strconv.FormatUint(uint64(held.Fd()), 10), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func nativeTempScopes(ctx context.Context) ([]ObservedScope, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify("temp_lookup", err)
	}
	return canonicalTempScopes([]ObservedScope{{Kind: SharedTemp, Name: "Shared temporary directory", Directory: "/tmp"}})
}
