package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func scheduleCreateArgs(t *testing.T) Arguments {
	t.Helper()
	parent := t.TempDir()
	cwd := filepath.Join(parent, "workspace")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(parent, "brief")
	if err := os.WriteFile(brief, []byte("requested brief"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Arguments{Root: filepath.Join(parent, "state"), Cwd: cwd, Brief: brief, Auto: true, ScheduleCron: "0 9 * * *"}
}

func TestScheduleCreateInputPreservesRequestedChoices(t *testing.T) {
	a := scheduleCreateArgs(t)
	a.Auto = false
	a.Provider = "codex:exec"
	a.Config = task.TaskConfig{Model: "exact-model", Effort: "high", Permission: "read-only", Budget: "60m", NativeTimeout: "30m"}
	a.ReviewTarget = "base:main"
	a.ScheduleTimezoneSet, a.ScheduleTimezone = true, "UTC"
	instant := time.Date(2026, 10, 31, 12, 34, 56, 123, time.UTC)
	clockCalls, idCalls := 0, 0
	input, err := prepareScheduleCreate(a, func() time.Time { clockCalls++; return instant }, func() (string, error) { idCalls++; return scheduleTestID, nil })
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := a.Config
	wantConfig.ReviewTarget = &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"}
	wantCwd, err := os.Stat(a.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	gotCwd, err := os.Stat(input.Request.CanonicalCwd)
	if err != nil || !os.SameFile(gotCwd, wantCwd) || !task.IsCanonicalCwd(input.Request.CanonicalCwd) {
		t.Fatalf("captured cwd %q does not identify requested directory: %v", input.Request.CanonicalCwd, err)
	}
	assertScheduleCapturedValues(t, input, a, wantConfig, instant, clockCalls, idCalls)
	if _, err := os.Lstat(a.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation created state root or failed to inspect it: %v", err)
	}
}

func assertScheduleCapturedValues(t *testing.T, input scheduleCreateInput, a Arguments, wantConfig task.TaskConfig, instant time.Time, clockCalls, idCalls int) {
	t.Helper()
	if input.ID != scheduleTestID || !input.CreatedAt.Equal(instant) || clockCalls != 1 || idCalls != 1 || !reflect.DeepEqual(input.Request.Config, wantConfig) || input.Request.Provider != a.Provider || input.Request.Auto || input.Cron != a.ScheduleCron || !input.TimezoneSet || input.Timezone != "UTC" || !bytes.Equal(input.Brief, []byte("requested brief")) {
		t.Fatalf("unexpected captured input: %+v clock=%d id=%d", input, clockCalls, idCalls)
	}
}

func TestScheduleCreateInputOmissionsAndExplicitID(t *testing.T) {
	a := scheduleCreateArgs(t)
	a.ScheduleID = scheduleTestID
	clockCalls := 0
	input, err := prepareScheduleCreate(a, func() time.Time { clockCalls++; return time.Now() }, func() (string, error) {
		t.Fatal("explicit ID invoked generator")
		return "", nil
	})
	if err != nil || input.ID != scheduleTestID || clockCalls != 1 || input.TimezoneSet || input.Request.Config != (task.TaskConfig{}) || !input.Request.Auto {
		t.Fatalf("input=%+v clock=%d err=%v", input, clockCalls, err)
	}
}

func TestScheduleCreateInputRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Arguments)
	}{
		{"missing brief", func(a *Arguments) { a.Brief = "" }},
		{"missing cwd", func(a *Arguments) { a.Cwd = "" }},
		{"overlap", func(a *Arguments) { a.Root = filepath.Join(a.Cwd, "state") }},
		{"missing cron", func(a *Arguments) { a.ScheduleCron = "" }},
		{"explicit empty timezone", func(a *Arguments) { a.ScheduleTimezoneSet = true }},
		{"both selections", func(a *Arguments) { a.Provider = "codex:exec" }},
		{"neither selection", func(a *Arguments) { a.Auto = false }},
		{"invalid permission", func(a *Arguments) { a.Config.Permission = "not-a-mode" }},
		{"invalid native timeout", func(a *Arguments) { a.Config.Budget, a.Config.NativeTimeout = "1m", "2m" }},
		{"invalid review target", func(a *Arguments) { a.ReviewTarget = "base:" }},
		{"invalid ID", func(a *Arguments) { a.ScheduleID = "bad" }},
		{"interactive", func(a *Arguments) { a.Config.Interactive = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := scheduleCreateArgs(t)
			tc.change(&a)
			_, err := prepareScheduleCreate(a, time.Now, func() (string, error) { return scheduleTestID, nil })
			if err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

func TestScheduleCreateInputUsesBriefOwner(t *testing.T) {
	a := scheduleCreateArgs(t)
	link := filepath.Join(filepath.Dir(a.Brief), "brief-link")
	if err := os.Symlink(a.Brief, link); err != nil {
		t.Fatal(err)
	}
	a.Brief = link
	_, err := prepareScheduleCreate(a, time.Now, func() (string, error) { return scheduleTestID, nil })
	if !errors.Is(err, config.ErrBriefNotRegular) {
		t.Fatalf("symlink brief err=%v", err)
	}
	a.Brief = filepath.Join(filepath.Dir(link), "empty")
	if writeErr := os.WriteFile(a.Brief, nil, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	_, err = prepareScheduleCreate(a, time.Now, func() (string, error) { return scheduleTestID, nil })
	if err == nil || !strings.Contains(err.Error(), "nonempty") {
		t.Fatalf("empty brief err=%v", err)
	}
	if truncateErr := os.Truncate(a.Brief, task.MaxBriefSize+1); truncateErr != nil {
		t.Fatal(truncateErr)
	}
	_, err = prepareScheduleCreate(a, time.Now, func() (string, error) { return scheduleTestID, nil })
	if !errors.Is(err, config.ErrBriefTooLarge) {
		t.Fatalf("oversized brief err=%v", err)
	}
}

func TestScheduleCreateInputIdentityErrors(t *testing.T) {
	a := scheduleCreateArgs(t)
	idFault := errors.New("random source unavailable")
	clockCalls, idCalls := 0, 0
	_, err := prepareScheduleCreate(a, func() time.Time { clockCalls++; return time.Now() }, func() (string, error) { idCalls++; return "", idFault })
	if !errors.Is(err, idFault) || idCalls != 1 || clockCalls != 0 {
		t.Fatalf("id fault err=%v id calls=%d clock calls=%d", err, idCalls, clockCalls)
	}
	_, err = prepareScheduleCreate(a, func() time.Time { clockCalls++; return time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC) }, func() (string, error) { idCalls++; return scheduleTestID, nil })
	if err == nil || idCalls != 2 || clockCalls != 1 {
		t.Fatalf("out-of-range instant err=%v id calls=%d clock calls=%d", err, idCalls, clockCalls)
	}
}
