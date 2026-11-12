package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func hookNotice() contract.Notification {
	return contract.Notification{EventID: strings.Repeat("a", 32), ScheduleID: strings.Repeat("b", 32), Kind: "failure", AnchorTaskID: strings.Repeat("c", 32), LastTaskID: strings.Repeat("d", 32), StreakCount: 3, FailureCode: "provider_quota_exhausted"}
}

func TestSchedulerHookContextAndEnvironmentAreBounded(t *testing.T) {
	data, err := schedulerHookContext(hookNotice())
	if err != nil {
		t.Fatal(err)
	}
	var contextRecord map[string]any
	if err = json.Unmarshal(data, &contextRecord); err != nil {
		t.Fatal(err)
	}
	if len(contextRecord) != 6 || contextRecord["streak_count"] != float64(3) || contextRecord["failure_code"] != "provider_quota_exhausted" {
		t.Fatalf("unexpected hook context %s", data)
	}
	selected := schedulerHookEnvironment([]string{"PATH=/bin", "HOME=/home/test", "PROVIDER_API_KEY=secret", "LANG=C", "LC_ALL=C", "TMPDIR=/tmp", "DELEGATE_ROOT=/private"})
	if !slices.Equal(selected, []string{"PATH=/bin", "HOME=/home/test", "LANG=C", "LC_ALL=C", "TMPDIR=/tmp"}) {
		t.Fatalf("unexpected hook environment keys: %d", len(selected))
	}
	invalid := hookNotice()
	invalid.FailureCode = "provider_error\nsecret"
	if _, err = schedulerHookContext(invalid); err == nil {
		t.Fatal("unsafe diagnostic accepted")
	}
}

func TestSchedulerHookRunsThroughNativeOwner(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	hooks := schedulerHooks{dependencies: Dependencies{RunnerExecutable: self, Environment: []string{"PATH=/bin:/usr/bin"}}}
	for _, tc := range []struct{ name, path, want string }{{"success", requiredHookTestExecutable(t, "true"), "succeeded"}, {"failure", requiredHookTestExecutable(t, "false"), "failed"}, {"missing", filepath.Join(t.TempDir(), "missing"), "unavailable"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, runErr := hooks.RunHook(ctx, contract.FailureHook{Executable: tc.path, Threshold: 1, Timeout: 3 * time.Second}, hookNotice())
			if runErr != nil || got != tc.want {
				t.Fatalf("hook result %q, error %v; want %q", got, runErr, tc.want)
			}
		})
	}
}

func TestSchedulerHookTimeoutDoesNotJoinInheritedOutput(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hook")
	// An ordinary descendant inherits stdout/stderr. Null output must not add
	// capture goroutines that wait for those descriptors after the deadline.
	if err = os.WriteFile(path, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, runErr := (schedulerHooks{dependencies: Dependencies{RunnerExecutable: self, Environment: []string{"PATH=/bin:/usr/bin"}}}).RunHook(ctx, contract.FailureHook{Executable: path, Threshold: 1, Timeout: 250 * time.Millisecond}, hookNotice())
	if runErr != nil || got != "timed_out" {
		t.Fatalf("hook result %q error %v", got, runErr)
	}
}

func TestSchedulerHookMissingRunnerRefusesBeforeCallback(t *testing.T) {
	directory := t.TempDir()
	callback := filepath.Join(directory, "notify")
	if err := os.WriteFile(callback, []byte("#!/bin/sh\n: > \"$0.called\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(directory, "private-missing-runner")
	hooks := schedulerHooks{dependencies: Dependencies{RunnerExecutable: missing}}
	got, err := hooks.RunHook(t.Context(), contract.FailureHook{Executable: callback, Threshold: 1, Timeout: time.Second}, hookNotice())
	if got != "unavailable" || err == nil || strings.Contains(err.Error(), directory) {
		t.Fatalf("missing runner result %q, error %v", got, err)
	}
	if _, statErr := os.Stat(callback + ".called"); !os.IsNotExist(statErr) {
		t.Fatalf("callback ran before runner resolution: %v", statErr)
	}
}

func requiredHookTestExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("required installed hook test command %s: %v", name, err)
	}
	return path
}
