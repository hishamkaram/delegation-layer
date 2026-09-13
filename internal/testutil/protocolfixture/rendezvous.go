package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxRendezvousBytes = 4096

// A created file is not a receipt: WriteFile creates it before writing its line.
// Only a complete newline-framed PID:label record acknowledges a checkpoint.
func readRendezvous(path, point string, expectedPID int) (int, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, errors.New("checkpoint receipt is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxRendezvousBytes+1))
	if err = errors.Join(readErr, f.Close()); err != nil {
		return 0, false, err
	}
	return parseRendezvous(data, point, expectedPID)
}

func parseRendezvous(data []byte, point string, expectedPID int) (int, bool, error) {
	if len(data) > maxRendezvousBytes {
		return 0, false, errors.New("checkpoint receipt exceeds size limit")
	}
	newline := bytes.IndexByte(data, '\n')
	if newline < 0 {
		return 0, false, nil
	}
	if newline != len(data)-1 {
		return 0, false, fmt.Errorf("checkpoint receipt has extra data: %q", data)
	}
	pidText, label, found := strings.Cut(string(data[:newline]), ":")
	pid, err := strconv.Atoi(pidText)
	if !found || err != nil || pid <= 0 || strconv.Itoa(pid) != pidText {
		return 0, false, fmt.Errorf("checkpoint receipt has invalid PID: %q", data)
	}
	if label != point || (expectedPID != 0 && pid != expectedPID) {
		return 0, false, fmt.Errorf("checkpoint receipt mismatch: got %d:%q, want %d:%q", pid, label, expectedPID, point)
	}
	return pid, true, nil
}

func waitForRendezvous(path, point string, expectedPID int, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	read := func() (int, bool, error) { return readRendezvous(path, point, expectedPID) }
	pid, err := pollRendezvous(ctx, read, ticker.C)
	if err != nil {
		return 0, fmt.Errorf("waiting for checkpoint at %q: %w", path, err)
	}
	return pid, nil
}

// The reader and tick channel permit deterministic, stepped handshake tests.
// Production uses the same finite deadline and ticker for all receipt consumers.
func pollRendezvous(ctx context.Context, read func() (int, bool, error), ticks <-chan time.Time) (int, error) {
	for {
		pid, complete, err := read()
		if err != nil {
			return 0, err
		}
		if complete {
			return pid, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticks:
		}
	}
}
