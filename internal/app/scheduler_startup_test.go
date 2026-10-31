package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSchedulerStartupMissingRootDoesNotInitialize(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	response, code := schedulerStartupJSON(t, schedulerStartupContext(t), root)
	if code != 1 || !strings.Contains(response.Error, "create a schedule first") {
		t.Fatalf("missing root code=%d response=%+v", code, response)
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("scheduler created the missing root or inspection failed: %v", err)
	}
}

func TestSchedulerStartupMissingDatabaseDoesNotInitialize(t *testing.T) {
	for _, tc := range []struct {
		name            string
		directoryExists bool
	}{
		{name: "without scheduler directory"},
		{name: "with empty scheduler directory", directoryExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := schedulerStartupState(t)
			directory := filepath.Join(root, ".scheduler")
			if tc.directoryExists {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			response, code := schedulerStartupJSON(t, schedulerStartupContext(t), root)
			if code != 1 || !strings.Contains(response.Error, "create a schedule first") {
				t.Fatalf("missing database code=%d response=%+v", code, response)
			}
			if _, err := os.Lstat(filepath.Join(directory, "schedules.sqlite")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("scheduler created the missing database or inspection failed: %v", err)
			}
			requireSchedulerStartupDirectory(t, directory, tc.directoryExists)
		})
	}
}

func schedulerStartupState(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	state, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func requireSchedulerStartupDirectory(t *testing.T, directory string, existed bool) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(directory, "owner.lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scheduler created an owner lock before a schedule exists or inspection failed: %v", err)
	}
	if !existed {
		if _, err := os.Lstat(directory); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("scheduler created its directory before a schedule exists: %v", err)
		}
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scheduler changed the empty directory: entries=%v err=%v", entries, err)
	}
}

func TestSchedulerStartupMissingStorageExplainsHumanRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	var stdout, stderr bytes.Buffer
	code := RunContext(schedulerStartupContext(t), []string{"scheduler", "run", "--root", root}, &stdout, &stderr, schedulerStartupDependencies(t))
	if code != 1 || !strings.Contains(stderr.String(), "create a schedule first") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("scheduler created the missing root or inspection failed: %v", err)
	}
}

func TestSchedulerStartupRefusesBusyOwnerLease(t *testing.T) {
	root, schedules, definition := scheduleFixture(t)
	state, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, err := scheduler.AcquireOwnerLease(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	response, code := schedulerStartupJSON(t, schedulerStartupContext(t), root)
	if code != 1 || !strings.Contains(response.Error, task.ErrLockBusy.Error()) || strings.Contains(response.Error, "create a schedule first") {
		t.Fatalf("busy owner code=%d response=%+v", code, response)
	}
	stored, err := schedules.GetSchedule(t.Context(), definition.ScheduleID)
	if err != nil || stored.State != contract.StateEnabled || !stored.Cursor.Equal(definition.CreatedAt) {
		t.Fatalf("busy startup changed schedule: %+v err=%v", stored, err)
	}
	history, err := schedules.History(t.Context(), contract.HistoryPage{ScheduleID: definition.ScheduleID, Limit: 1})
	if err != nil || history.More || len(history.Entries) != 0 {
		t.Fatalf("busy startup recorded history: %+v err=%v", history, err)
	}
}

func TestSchedulerStartupContextFailureIsNotCleanShutdown(t *testing.T) {
	root, _, _ := scheduleFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		code int
		want string
	}{
		{"missing context", nil, 2, "scheduler requires a context"},
		{"canceled context", canceled, 1, context.Canceled.Error()},
		{"expired context", expired, 1, context.DeadlineExceeded.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, code := schedulerStartupJSON(t, tc.ctx, root)
			if code != tc.code || !strings.Contains(response.Error, tc.want) || strings.Contains(response.Error, "create a schedule first") {
				t.Fatalf("startup context code=%d response=%+v; want code=%d error=%q", code, response, tc.code, tc.want)
			}
		})
	}
}

func schedulerStartupContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func schedulerStartupJSON(t *testing.T, ctx context.Context, root string) (Response, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunContext(ctx, []string{"scheduler", "run", "--root", root, "--json"}, &stdout, &stderr, schedulerStartupDependencies(t))
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v; stderr=%q", stdout.String(), err, stderr.String())
	}
	if response.SchemaVersion != OutputSchemaVersion || response.Command != "scheduler" || stderr.Len() != 0 {
		t.Fatalf("startup response=%+v stderr=%q", response, stderr.String())
	}
	return response, code
}

func schedulerStartupDependencies(t *testing.T) Dependencies {
	t.Helper()
	var providerCalls, supervisorCalls atomic.Int64
	t.Cleanup(func() {
		if providerCalls.Load() != 0 || supervisorCalls.Load() != 0 {
			t.Errorf("startup probed providers or supervisor: provider=%d supervisor=%d", providerCalls.Load(), supervisorCalls.Load())
		}
	})
	return Dependencies{
		Environment: []string{"HOME=" + t.TempDir(), "PATH=" + t.TempDir()},
		PrepareCandidate: func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
			providerCalls.Add(1)
			return commonprovider.ProfileCandidate{}, errors.New("unexpected provider preparation during scheduler startup")
		},
		SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { supervisorCalls.Add(1) }},
	}
}
