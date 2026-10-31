package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestCheckPrivateStatDecisions(t *testing.T) {
	uid := uint32(os.Geteuid())
	cases := map[string]struct {
		stat      unix.Stat_t
		directory bool
		want      PrivateStatVerdict
	}{
		"private file":       {stat: unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: uid}, want: PrivateStatAccepted},
		"private directory":  {stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: uid}, directory: true, want: PrivateStatAccepted},
		"other owner":        {stat: unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: uid + 1}, want: PrivateStatUnsafeOwnerOrMode},
		"group readable":     {stat: unix.Stat_t{Mode: unix.S_IFREG | 0o640, Uid: uid}, want: PrivateStatUnsafeOwnerOrMode},
		"read only":          {stat: unix.Stat_t{Mode: unix.S_IFREG | 0o400, Uid: uid}, want: PrivateStatUnsafeOwnerOrMode},
		"setuid file":        {stat: unix.Stat_t{Mode: unix.S_IFREG | unix.S_ISUID | 0o600, Uid: uid}, want: PrivateStatUnsafeOwnerOrMode},
		"setgid directory":   {stat: unix.Stat_t{Mode: unix.S_IFDIR | unix.S_ISGID | 0o700, Uid: uid}, directory: true, want: PrivateStatUnsafeOwnerOrMode},
		"sticky directory":   {stat: unix.Stat_t{Mode: unix.S_IFDIR | unix.S_ISVTX | 0o700, Uid: uid}, directory: true, want: PrivateStatUnsafeOwnerOrMode},
		"file mode on dir":   {stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o600, Uid: uid}, directory: true, want: PrivateStatUnsafeOwnerOrMode},
		"directory not file": {stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o600, Uid: uid}, want: PrivateStatUnexpectedType},
		"file not directory": {stat: unix.Stat_t{Mode: unix.S_IFREG | 0o700, Uid: uid}, directory: true, want: PrivateStatUnexpectedType},
		"symlink":            {stat: unix.Stat_t{Mode: unix.S_IFLNK | 0o600, Uid: uid}, want: PrivateStatUnexpectedType},
		"fifo":               {stat: unix.Stat_t{Mode: unix.S_IFIFO | 0o600, Uid: uid}, want: PrivateStatUnexpectedType},
		"owner judged first": {stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o644, Uid: uid}, want: PrivateStatUnsafeOwnerOrMode},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := CheckPrivateStat(&tc.stat, os.Geteuid(), tc.directory); got != tc.want {
				t.Fatalf("verdict %d, want %d", got, tc.want)
			}
		})
	}
}

func TestValidatePrivateFileKeepsItsMessages(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path      string
		mode      os.FileMode
		directory bool
		message   string
	}{
		{path: file, mode: 0o600},
		{path: file, mode: 0o644, message: "unsafe owner or mode on " + file},
		{path: private, mode: 0o600, message: "unexpected file type on " + private},
	}
	for _, tc := range cases {
		if err := os.Chmod(tc.path, tc.mode); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		err = validatePrivateFile(f, tc.directory)
		closeFileQuietly(f)
		switch {
		case tc.message == "" && err != nil:
			t.Errorf("%s: %v", tc.path, err)
		case tc.message != "" && (!errors.Is(err, task.ErrEvidenceFault) || !strings.HasSuffix(err.Error(), tc.message)):
			t.Errorf("%s err=%v, want evidence fault %q", tc.path, err, tc.message)
		}
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
}
