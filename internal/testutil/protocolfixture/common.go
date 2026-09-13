package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Deferred cleanup must not turn a failed fixture observation into success.
var cleanupFailed atomic.Bool

func reportCleanup(err error) {
	if err != nil {
		cleanupFailed.Store(true)
		fmt.Fprintf(os.Stderr, "fixture cleanup: %v\n", err)
	}
}

func closeStore(s *taskdir.Store) {
	if s != nil {
		reportCleanup(s.Close())
	}
}

func closeTask(t *taskdir.TaskDir) {
	if t != nil {
		reportCleanup(t.Close())
	}
}

func closeLock(l *taskdir.LockFile) {
	if l != nil {
		reportCleanup(l.Close())
	}
}

func closeFile(f *os.File) {
	if f != nil {
		reportCleanup(f.Close())
	}
}

func signalRendezvous(path, msg string) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d:%s\n", os.Getpid(), msg)), 0o600)
}

func signalRendezvousQuietly(path, msg string) {
	// Callers without an immediate return still fail through command cleanup status.
	reportCleanup(signalRendezvous(path, msg))
}

func recordFakeSinkEvent(path, event, taskID string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	entry := fmt.Sprintf("%d:%s:%s\n", os.Getpid(), event, taskID)
	n, writeErr := io.WriteString(f, entry)
	if writeErr == nil && n != len(entry) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, f.Close())
}

func printJSON(v any) int {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "writing fixture result: %v\n", err)
		return 1
	}
	return 0
}

func readControlFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("control fixture must be regular")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := task.ReadControlRecord(f)
	return data, errors.Join(readErr, f.Close())
}
