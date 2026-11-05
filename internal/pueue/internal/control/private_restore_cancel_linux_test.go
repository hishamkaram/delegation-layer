//go:build linux

package control

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestObservePrivateRestoreCancellationDuringNativeReadIsOperational(t *testing.T) {
	f := newPrivateRestoreFixture(t, false)
	writePrivateRestoreBytes(t, f.state, privateRestoreLargeQueue(t, f.bytes))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	notified := cancelPrivateRestoreOnRead(t, f.state, cancel)
	got, err := ObservePrivateRestore(ctx, f.pin, f.binding, f.resolution)
	select {
	case <-notified:
	default:
		t.Fatal("public observation ended before the native state-read cancellation gate")
	}
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrPrivateRestoreCorrupt) || errors.Is(err, ErrPrivateRestoreLayout) || got.Presence() != 0 {
		t.Fatalf("read cancellation classified as damaged restore: presence=%v err=%v", got.Presence(), err)
	}
}

func privateRestoreLargeQueue(t *testing.T, source []byte) []byte {
	t.Helper()
	var document struct {
		Tasks  map[string]map[string]json.RawMessage `json:"tasks"`
		Groups json.RawMessage                       `json:"groups"`
	}
	if err := json.Unmarshal(source, &document); err != nil {
		t.Fatal(err)
	}
	row := document.Tasks["0"]
	var result bytes.Buffer
	result.WriteString(`{"tasks":{`)
	for id := range 8192 {
		if id != 0 {
			result.WriteByte(',')
		}
		key := strconv.Itoa(id)
		row["id"] = json.RawMessage(key)
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		result.WriteString(strconv.Quote(key))
		result.WriteByte(':')
		result.Write(data)
	}
	result.WriteString(`},"groups":`)
	result.Write(document.Groups)
	result.WriteByte('}')
	return result.Bytes()
}

func cancelPrivateRestoreOnRead(t *testing.T, path string, cancel context.CancelFunc) <-chan struct{} {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	watchContext, stop := context.WithCancel(t.Context())
	done, notified := make(chan struct{}), make(chan struct{})
	started := false
	t.Cleanup(func() {
		stop()
		if started {
			<-done
		}
		if closeErr := unix.Close(fd); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = unix.InotifyAddWatch(fd, path, unix.IN_ACCESS); err != nil {
		t.Fatal(err)
	}
	started = true
	go func() {
		defer close(done)
		if readErr := waitPrivateRestoreRead(watchContext, fd); readErr != nil {
			if !errors.Is(readErr, context.Canceled) {
				t.Error(readErr)
			}
			return
		}
		close(notified)
		cancel()
	}()
	return notified
}

func waitPrivateRestoreRead(ctx context.Context, fd int) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var buffer [4096]byte
	for {
		n, err := unix.Read(fd, buffer[:])
		if err == nil && n >= unix.SizeofInotifyEvent && binary.NativeEndian.Uint32(buffer[4:8])&unix.IN_ACCESS != 0 {
			return nil
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
