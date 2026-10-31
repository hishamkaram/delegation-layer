package tasklifecycle

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"golang.org/x/sys/unix"
)

func TestReadBriefUsesNoFollowNonblockingOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brief.md")
	want := []byte("brief\nwith exact bytes")
	writeAppTestFile(t, path, want)
	f, err := openBriefForRead(path)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("valid brief read changed: bytes=%q read=%v close=%v", got, readErr, closeErr)
	}
	link := filepath.Join(dir, "brief-link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = openBriefForRead(link); !errors.Is(err, config.ErrBriefNotRegular) {
		t.Fatalf("symlink brief was followed: %v", err)
	}
	fifo := filepath.Join(dir, "brief.fifo")
	if err = unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, openErr := openBriefForRead(fifo)
		result <- openErr
	}()
	select {
	case err = <-result:
		if !errors.Is(err, config.ErrBriefNotRegular) {
			t.Fatalf("FIFO result=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO open blocked")
	}
}

func writeAppTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyBriefHasSharedInputIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.md")
	writeAppTestFile(t, path, nil)
	if data, err := ReadBrief(path); len(data) != 0 || !errors.Is(err, config.ErrBriefEmpty) {
		t.Fatalf("empty file bytes=%q err=%v; want empty-brief identity", data, err)
	}
	request := DispatchRequest{Root: filepath.Join(t.TempDir(), "state"), Cwd: t.TempDir(), BriefBytes: []byte{}}
	if _, _, _, err := dispatchInputs(request); !errors.Is(err, config.ErrBriefEmpty) {
		t.Fatalf("empty supplied brief err=%v; want empty-brief identity", err)
	}
}
