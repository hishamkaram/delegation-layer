//go:build darwin

package workspacefiles

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/execution"
)

func changeTime(stat *syscall.Stat_t) string {
	return strconv.FormatInt(stat.Ctimespec.Sec, 10) + ":" + strconv.FormatInt(stat.Ctimespec.Nsec, 10)
}

func openContent(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func nativeTempScopes(ctx context.Context) ([]ObservedScope, error) {
	owner, err := os.Executable()
	if err != nil {
		return nil, failure(TempUnavailable, "temp_lookup", err)
	}
	// The installed Native helper owner supplies one Start/Wait and joins its
	// streams. Neither TMPDIR nor remotely controlled argv/env selects the root.
	command := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR")
	command.Dir = "/"
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	output := &boundedOutput{limit: 4096}
	command.Stdout = output
	command.Stderr = io.Discard
	if err = execution.RunIsolatedCommand(ctx, owner, command); err != nil || output.overflow {
		return nil, failure(TempUnavailable, "temp_lookup", err)
	}
	path := strings.TrimSuffix(string(output.bytes), "\n")
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return nil, failure(TempUnavailable, "temp_lookup", nil)
	}
	return canonicalTempScopes([]ObservedScope{
		{Kind: UserTemp, Name: "User temporary directory", Directory: path},
		{Kind: SharedTemp, Name: "Shared temporary directory", Directory: "/tmp"},
	})
}

type boundedOutput struct {
	bytes    []byte
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	available := b.limit - len(b.bytes)
	if len(data) > available {
		b.overflow = true
		b.bytes = append(b.bytes, data[:available]...)
	} else {
		b.bytes = append(b.bytes, data...)
	}
	return len(data), nil
}
