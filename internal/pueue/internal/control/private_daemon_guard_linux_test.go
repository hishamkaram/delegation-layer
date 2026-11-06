//go:build linux

package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDaemonGuardStartedChildAlwaysReaped(t *testing.T) {
	postStart := errors.New("placement completion failed")
	for _, tc := range []struct {
		name   string
		twice  bool
		result error
	}{
		{name: "success"}, {name: "post-start failure", result: postStart}, {name: "duplicate ignored", twice: true, result: ErrInfrastructureOwnerRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			pidPath := filepath.Join(root, "pid")
			joined := false
			// The fixture exits without external work; cleanup verifies its exact PID is reaped.
			t.Cleanup(func() {
				if !joined {
					waitDaemonGuardChildReaped(t, pidPath)
				}
			})
			executable := filepath.Join(root, "daemon")
			writeExecutable(t, executable, "#!/bin/sh\nprintf '%s\\n' \"$$\" >> '"+pidPath+"'\n")
			guard := daemonGuardReapingFixture(t, tc.twice, tc.result)
			err := startDaemon(t.Context(), root, executable, filepath.Join(root, "config"), root, Options{DaemonStartGuard: guard})
			if tc.result == nil && err != nil || tc.result != nil && !errors.Is(err, tc.result) {
				t.Fatalf("start result = %v, want %v", err, tc.result)
			}
			waitDaemonGuardChildReaped(t, pidPath)
			joined = true
		})
	}
}

func daemonGuardReapingFixture(t *testing.T, twice bool, result error) DaemonStartGuard {
	t.Helper()
	return daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		if startErr := start(); startErr != nil {
			return startErr
		}
		if twice {
			if secondErr := start(); !errors.Is(secondErr, ErrInfrastructureOwnerRequired) {
				t.Errorf("second callback = %v", secondErr)
			}
			return nil
		}
		return result
	})
}

func TestDaemonGuardPIDPublication(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		pid  int
		bad  bool
	}{
		{name: "empty file before printf"},
		{name: "shell PID line", data: "42\n", pid: 42},
		{name: "existing caller PID", data: "42", pid: 42},
		{name: "whitespace is not a publication", data: " \n", bad: true},
		{name: "duplicate starts", data: "42\n43\n", bad: true},
		{name: "malformed PID", data: "child\n", bad: true},
		{name: "zero PID", data: "0\n", bad: true},
		{name: "negative PID", data: "-42\n", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, err := daemonGuardPublishedPID([]byte(tc.data))
			if (err != nil) != tc.bad || pid != tc.pid {
				t.Fatalf("PID publication %q = %d, %v; want PID %d, error=%t", tc.data, pid, err, tc.pid, tc.bad)
			}
		})
	}
}

func daemonGuardPublishedPID(data []byte) (int, error) {
	// Append redirection creates the file before printf publishes its PID.
	if len(data) == 0 {
		return 0, nil
	}
	pids := strings.Fields(string(data))
	if len(pids) != 1 {
		return 0, fmt.Errorf("daemon starts = %d, want 1", len(pids))
	}
	pid, err := strconv.Atoi(pids[0])
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid child PID %q", pids[0])
	}
	return pid, nil
}

func waitDaemonGuardChildReaped(t *testing.T, pidPath string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if errors.Is(err, os.ErrNotExist) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		pid, err := daemonGuardPublishedPID(data)
		if err != nil {
			t.Fatal(err)
		}
		if pid == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		_, err = os.Stat(fmt.Sprintf("/proc/%d", pid))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("exact finite child was not reaped")
}
