package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestScheduleDefaultIsSharedAcrossWorkingDirectoriesAndIgnoresAmbientRoot(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("HOME", profile)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(profile, "config"))
	invented := filepath.Join(t.TempDir(), "invented")
	t.Setenv("DELEGATE_ROOT", invented)
	args := scheduleCreateArgs(t)
	t.Chdir(args.Cwd)
	var stdout, stderr bytes.Buffer
	// Drop the helper's explicit root to exercise ordinary public CLI resolution.
	code := Run(scheduleCreateArgv(args)[2:], &stdout, &stderr, Dependencies{})
	var created Response
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	assertScheduleStartRequired(t, code, created)
	t.Chdir(t.TempDir())
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"schedule", "list", "--json"}, &stdout, &stderr, Dependencies{})
	var listed Response
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if code != 0 || listed.RootID != created.RootID || listed.Schedules == nil || len(*listed.Schedules) != 1 || (*listed.Schedules)[0].ID != scheduleTestID {
		t.Fatalf("cwd changed the default store: code=%d response=%+v", code, listed)
	}
	if _, err := os.Lstat(invented); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ambient DELEGATE_ROOT selected state: %v", err)
	}
}
