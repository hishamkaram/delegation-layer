//go:build linux || darwin

package remotefiles

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadOpenDoesNotBlockOnConcurrentFIFOReplacement(t *testing.T) {
	root := rooted(t)
	write(t, root, "selected", "regular")
	before, err := root.Lstat("selected")
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Remove("selected"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root.Name(), "selected")
	if err = syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		file *os.File
		err  error
	}
	opened := make(chan result, 1)
	go func() { file, openErr := openReadOnly(root, "selected"); opened <- result{file, openErr} }()
	var observed result
	select {
	case observed = <-opened:
	case <-time.After(time.Second):
		// Unblock a defective blocking reader before failing; the test owns this FIFO.
		peer, openErr := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
		if openErr != nil {
			t.Fatalf("release blocked FIFO reader: %v", openErr)
		}
		observed = <-opened
		closeErr := syscall.Close(peer)
		if observed.file != nil {
			closeErr = errors.Join(closeErr, observed.file.Close())
		}
		t.Fatalf("native read open blocked on FIFO replacement (cleanup=%v)", closeErr)
	}
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	defer func() {
		if closeErr := observed.file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	after, err := observed.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	var invalid *sourceValidityError
	if err = validateOpenedRegular("selected", before, after); !errors.As(err, &invalid) || invalid.reason != "changed" {
		t.Fatalf("replacement descriptor was accepted: %v", err)
	}
}

func TestOpenedRegularRechecksSameInodeLinkCount(t *testing.T) {
	root := rooted(t)
	write(t, root, "selected", "regular")
	before, err := root.Lstat("selected")
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Link("selected", "added-link"); err != nil {
		t.Fatal(err)
	}
	file, err := openReadOnly(root, "selected")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	after, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("test did not preserve native file identity")
	}
	var invalid *sourceValidityError
	if err = validateOpenedRegular("selected", before, after); !errors.As(err, &invalid) || invalid.reason != "hard_link" {
		t.Fatalf("new hard link was accepted: %v", err)
	}
}
