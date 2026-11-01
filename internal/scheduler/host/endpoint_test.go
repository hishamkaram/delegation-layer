package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHostEndpointClosePreservesReplacedSocket(t *testing.T) {
	endpoint, err := newEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := endpoint.close(); closeErr != nil && !errors.Is(closeErr, ErrIdentity) {
			t.Error(closeErr)
		}
	})
	if err = endpoint.directory.Remove("s"); err != nil {
		t.Fatal(err)
	}
	file, err := endpoint.directory.OpenFile("s", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("replacement belongs to another owner")
	if err = errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	ownReplacementCleanup(t, endpoint)
	if err = endpoint.close(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("replaced endpoint was accepted: %v", err)
	}
	data, err := os.ReadFile(endpoint.path)
	if err != nil || string(data) != "replacement belongs to another owner" {
		t.Fatalf("replacement was removed or changed: %q %v", data, err)
	}
}

func ownReplacementCleanup(t *testing.T, endpoint *endpoint) {
	t.Helper()
	// The test owns this replacement file and separately pins its directory for
	// bounded cleanup. The production endpoint owns only its original socket.
	directory, err := os.OpenRoot(filepath.Dir(endpoint.path))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(filepath.Dir(filepath.Dir(endpoint.path)))
	if err != nil {
		t.Fatal(errors.Join(err, directory.Close()))
	}
	replacement, err := directory.Lstat("s")
	if err != nil {
		t.Fatal(errors.Join(err, directory.Close(), parent.Close()))
	}
	t.Cleanup(func() {
		current, statErr := directory.Lstat("s")
		if statErr == nil && os.SameFile(current, replacement) {
			statErr = directory.Remove("s")
		} else if statErr == nil {
			statErr = ErrIdentity
		}
		if statErr != nil {
			t.Error(statErr)
		}
		dirInfo, dirErr := parent.Lstat(endpoint.name)
		if dirErr == nil && os.SameFile(dirInfo, endpoint.dirInfo) {
			dirErr = parent.Remove(endpoint.name)
		} else if dirErr == nil {
			dirErr = ErrIdentity
		}
		if closeErr := errors.Join(directory.Close(), dirErr, parent.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
}
