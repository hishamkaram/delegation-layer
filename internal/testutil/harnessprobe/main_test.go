package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"golang.org/x/sys/unix"
)

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func createSparseTestFile(t *testing.T, path string, size int64) {
	t.Helper()
	file, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if truncateErr := file.Truncate(size); truncateErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		t.Fatal(truncateErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestParseRefusesFIFOWithoutBlocking(t *testing.T) {
	base, err := os.MkdirTemp("", "harnessprobe-fifo-*")
	if err != nil {
		t.Fatal(err)
	}
	joined := true // No worker has started during partial setup.
	t.Cleanup(func() {
		if !joined {
			t.Errorf("FIFO parser closure unverified; retained %s", base)
			return
		}
		if cleanupErr := os.RemoveAll(base); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(canonical, "config.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var parseErr error
	t.Cleanup(func() {
		if err := releaseFIFOParser(fifo, done); err != nil {
			t.Error(err)
			return
		}
		joined = true
	})
	joined = false
	go func() {
		_, parseErr = parse(fifo)
		close(done)
	}()
	select {
	case <-done:
		if parseErr == nil {
			t.Fatal("FIFO was accepted as a config")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FIFO config read blocked")
	}
}

// A regression may block either opening or reading the FIFO. A nonblocking
// writer rendezvous followed by EOF releases both without abandoning the worker.
func releaseFIFOParser(fifo string, done <-chan struct{}) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return nil
		default:
		}
		writer, err := os.OpenFile(fifo, os.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
		if err == nil {
			closeErr := writer.Close()
			select {
			case <-done:
				return closeErr
			case <-deadline.C:
				return errors.Join(closeErr, errors.New("FIFO parser did not join after release"))
			}
		}
		if !errors.Is(err, unix.ENXIO) {
			return err
		}
		select {
		case <-done:
			return nil
		case <-deadline.C:
			return errors.New("FIFO parser did not accept release")
		case <-tick.C:
		}
	}
}

func TestParseRefusesSymlinkedConfigAndParent(t *testing.T) {
	base := canonicalTestDir(t)
	targetDir := filepath.Join(base, "real")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "config.yml")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalLink := filepath.Join(base, "config-link.yml")
	if err := os.Symlink(target, finalLink); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(base, "real-link")
	if err := os.Symlink(targetDir, parentLink); err != nil {
		t.Fatal(err)
	}
	parentLinkedConfig := filepath.Join(parentLink, "config.yml")

	for name, path := range map[string]string{
		"final component":  finalLink,
		"parent component": parentLinkedConfig,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(path); err == nil {
				t.Fatalf("symlinked config path was accepted: %s", path)
			}
		})
	}
}

func TestParsePreservesCleanPathAndSizeChecks(t *testing.T) {
	base := canonicalTestDir(t)
	cleanPath := filepath.Join(base, "config.yml")
	if err := os.WriteFile(cleanPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	uncleanPath := base + string(filepath.Separator) + "." + string(filepath.Separator) + "config.yml"
	if _, err := parse(uncleanPath); err == nil {
		t.Fatal("unclean config path was accepted")
	}

	oversized := filepath.Join(base, "oversized.yml")
	createSparseTestFile(t, oversized, int64(pueue.MaxControlBytes)+1)
	if _, err := parse(oversized); err == nil {
		t.Fatal("oversized config was accepted")
	}
}
