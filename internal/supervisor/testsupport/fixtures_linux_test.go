package testsupport

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWriteExecutableExcludesForkWhileWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// A payload larger than the native pipe buffer keeps the writer's descriptor live
	// after the first byte is read, without a sleep or a scheduling assumption.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	reader := os.NewFile(uintptr(fd), path)
	var done chan struct{}
	result := make(chan error, 1)
	t.Cleanup(func() { closeExecutableWriter(t, reader, done, result) })
	deadline := time.Now().Add(10 * time.Second) // Hang watchdog, not a latency requirement.
	if parent, ok := t.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if deadlineErr := reader.SetReadDeadline(deadline); deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	capacity, err := unix.FcntlInt(uintptr(fd), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.Repeat("x", 2*capacity+1)
	done = make(chan struct{})
	go func() {
		defer close(done)
		result <- writeFixtureExecutable(path, contents)
		close(result)
	}()
	first := make([]byte, 1)
	if _, readErr := io.ReadFull(reader, first); readErr != nil {
		t.Fatal(readErr)
	}
	if syscall.ForkLock.TryLock() {
		syscall.ForkLock.Unlock()
		t.Error("fork admitted while the executable descriptor was writable")
	}
	if _, readErr := io.CopyN(io.Discard, reader, int64(len(contents)-1)); readErr != nil {
		t.Fatal(readErr)
	}
	<-done
	if writeErr := <-result; writeErr != nil {
		t.Fatal(writeErr)
	}
	if !syscall.ForkLock.TryLock() {
		t.Fatal("fork remains blocked after the executable write completed")
	}
	syscall.ForkLock.Unlock()
}

func closeExecutableWriter(t *testing.T, reader *os.File, done <-chan struct{}, result <-chan error) {
	t.Helper()
	// Keep the RDWR anchor open until even a late writer has opened and
	// finished. Closing it first could strand that writer in FIFO open.
	var drained chan error
	if done != nil {
		if deadlineErr := reader.SetReadDeadline(time.Time{}); deadlineErr != nil {
			t.Error(deadlineErr)
		}
		drained = make(chan error, 1)
		go func() {
			_, drainErr := io.Copy(io.Discard, reader)
			drained <- drainErr
		}()
		<-done
		if writeErr := <-result; writeErr != nil {
			t.Error(writeErr)
		}
	}
	if closeErr := reader.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	if drained != nil {
		if drainErr := <-drained; drainErr != nil && !errors.Is(drainErr, os.ErrClosed) {
			t.Error(drainErr)
		}
	}
}
