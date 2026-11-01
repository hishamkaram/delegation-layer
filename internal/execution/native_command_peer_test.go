package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type nativePeerRead struct {
	result nativeCommandResult
	err    error
}

func nativePeerPair(t *testing.T) (*net.UnixConn, *os.File) {
	t.Helper()
	connection, peer, err := nativeSocketPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		if closeErr := peer.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return connection, peer
}

func TestNativeCommandPeerRetainedWithoutCompletionTimesOut(t *testing.T) {
	var frame bytes.Buffer
	if err := writeNativeFrame(&frame, nativeCommandResult{ExitCode: 0, Exited: true}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"no header", nil},
		{"partial header", frame.Bytes()[:2]},
		{"partial body", frame.Bytes()[:frame.Len()-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connection, peer := nativePeerPair(t)
			if err := writeNativeBytes(peer, tc.data); err != nil {
				t.Fatal(err)
			}
			// Production owns a 30s cleanup allowance. Put this request's
			// deadline in the past so its real result-read bound is 100ms away.
			deadline := time.Now().Add(-30*time.Second + 100*time.Millisecond)
			completed := make(chan nativePeerRead, 1)
			go func() {
				result, err := readNativeCompletion(connection, deadline)
				completed <- nativePeerRead{result: result, err: err}
			}()
			select {
			case read := <-completed:
				var timeout net.Error
				if !errors.As(read.err, &timeout) || !timeout.Timeout() || !errors.Is(read.err, os.ErrDeadlineExceeded) {
					t.Fatalf("retained-peer result read = %+v, %v; want a typed deadline error", read.result, read.err)
				}
			case <-time.After(5 * time.Second):
				// This watchdog is not the read's latency contract. Release
				// only the test's endpoint and join the reader before failing.
				closeErr := connection.Close()
				read := <-completed
				t.Fatalf("result read ignored its bound while peer remained open: close=%v read=%v", closeErr, read.err)
			}
		})
	}
}

func TestNativeCommandPeerCompletionPreservesSemanticResult(t *testing.T) {
	connection, peer := nativePeerPair(t)
	want := nativeCommandResult{ExitCode: 128, Exited: true}
	if err := writeNativeFrame(peer, want); err != nil {
		t.Fatal(err)
	}
	got, err := readNativeCompletion(connection, time.Now())
	if err != nil || got != want {
		t.Fatalf("complete result with retained peer = %+v, %v; want %+v", got, err, want)
	}
}

type nativePeerSocket struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func nativePeerSocketAt(fd int) (nativePeerSocket, bool, error) {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return nativePeerSocket{}, false, err
	}
	return nativePeerSocket{Device: uint64(info.Dev), Inode: uint64(info.Ino)}, info.Mode&unix.S_IFMT == unix.S_IFSOCK, nil
}

func nativePeerInheritedSockets() ([]nativePeerSocket, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	sockets := []nativePeerSocket{}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("parse native descriptor: %w", err)
		}
		identity, socket, err := nativePeerSocketAt(fd)
		// ReadDir's own descriptor is already closed; a runtime descriptor
		// may also close between the inventory and fstat.
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if socket {
			sockets = append(sockets, identity)
		}
	}
	return sockets, nil
}

func TestNativeCommandPeerDescriptorHelper(t *testing.T) {
	if os.Getenv("DELEGATE_NATIVE_PEER_DESCRIPTORS") != "1" {
		return
	}
	sockets, err := nativePeerInheritedSockets()
	if err != nil {
		t.Fatal(err)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(sockets); encodeErr != nil {
		t.Fatal(encodeErr)
	}
	os.Exit(0)
}

type nativePeerSocketBatch struct {
	connections []*net.UnixConn
	peers       []*os.File
	identities  []nativePeerSocket
	err         error
}

func (batch *nativePeerSocketBatch) close() error {
	var resultErr error
	for _, connection := range batch.connections {
		resultErr = errors.Join(resultErr, connection.Close())
	}
	for _, peer := range batch.peers {
		resultErr = errors.Join(resultErr, peer.Close())
	}
	return resultErr
}

func nativePeerCreateBatch(count int) nativePeerSocketBatch {
	var batch nativePeerSocketBatch
	for range count {
		connection, peer, err := nativeSocketPair()
		if err != nil {
			batch.err = err
			return batch
		}
		batch.connections, batch.peers = append(batch.connections, connection), append(batch.peers, peer)
		control, err := connection.SyscallConn()
		if err != nil {
			batch.err = err
			return batch
		}
		var parent nativePeerSocket
		var inspectErr error
		if err = control.Control(func(fd uintptr) { parent, _, inspectErr = nativePeerSocketAt(int(fd)) }); err != nil || inspectErr != nil {
			batch.err = errors.Join(err, inspectErr)
			return batch
		}
		child, socket, err := nativePeerSocketAt(int(peer.Fd()))
		if err != nil || !socket || parent.Inode == 0 || child.Inode == 0 {
			batch.err = errors.Join(errors.New("IPC endpoints must have native socket identities"), err)
			return batch
		}
		batch.identities = append(batch.identities, parent, child)
	}
	return batch
}

type nativePeerSiblingResult struct {
	sockets []nativePeerSocket
	err     error
}

func nativePeerInspectSibling(executable, directory string) nativePeerSiblingResult {
	command := exec.Command(executable, "-test.run=^TestNativeCommandPeerDescriptorHelper$")
	command.Dir = directory
	command.Env = append(os.Environ(), "DELEGATE_NATIVE_PEER_DESCRIPTORS=1")
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nativePeerSiblingResult{err: err}
	}
	var sockets []nativePeerSocket
	if err := json.Unmarshal(output.Bytes(), &sockets); err != nil {
		return nativePeerSiblingResult{err: fmt.Errorf("decode sibling descriptor inventory: %w", err)}
	}
	return nativePeerSiblingResult{sockets: sockets}
}

func TestNativeCommandPeerConcurrentSiblingsDoNotInheritIPC(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor inventory needs native Linux procfs")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	batches, siblings := make([]nativePeerSocketBatch, 2), make([]nativePeerSiblingResult, 4)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := range batches {
		workers.Go(func() {
			<-start
			batches[index] = nativePeerCreateBatch(32)
		})
	}
	for index := range siblings {
		workers.Go(func() {
			<-start
			siblings[index] = nativePeerInspectSibling(executable, directory)
		})
	}
	close(start)
	workers.Wait() // Every sibling owns and joins its single native Run.
	for index := range batches {
		t.Cleanup(func() {
			if closeErr := batches[index].close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	requireNativePeerSiblingIsolation(t, batches, siblings)
}

func requireNativePeerSiblingIsolation(t *testing.T, batches []nativePeerSocketBatch, siblings []nativePeerSiblingResult) {
	t.Helper()
	owned := make(map[nativePeerSocket]bool)
	for _, batch := range batches {
		if batch.err != nil {
			t.Fatal(batch.err)
		}
		for _, identity := range batch.identities {
			owned[identity] = true
		}
	}
	for index, sibling := range siblings {
		if sibling.err != nil {
			t.Fatalf("sibling %d failed: %v", index, sibling.err)
		}
		for _, identity := range sibling.sockets {
			if owned[identity] {
				t.Errorf("concurrently spawned sibling %d inherited IPC socket %+v", index, identity)
			}
		}
	}
}
