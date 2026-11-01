package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerReaderRejectsUnknownDefinitionBeforeReadiness(t *testing.T) {
	root, schedules, definition := scheduleFixture(t)
	if err := checkSchedulerDefinitions(t.Context(), schedules); err != nil {
		t.Fatal(err)
	}
	// Only this fixture writes a future-format field directly. Production
	// commands keep the existing scheduler transaction/definition owners.
	database, err := sql.Open("sqlite", filepath.Join(root, ".scheduler", "schedules.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	var data []byte
	if err = database.QueryRow("SELECT request_json FROM schedules WHERE schedule_id = ?", definition.ScheduleID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var request map[string]json.RawMessage
	if err = json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	request["future_execution_mode"] = json.RawMessage(`true`)
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	future := definition.Clone()
	future.ScheduleID = strings.Repeat("b", 32)
	digests, err := contract.DefinitionDigests(future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`INSERT INTO schedules
		(schedule_id, created_at_unix_nanos, cron, timezone, request_json, brief,
		brief_sha256, definition_sha256, state, cursor_unix_nanos)
		SELECT ?, created_at_unix_nanos, cron, timezone, ?, brief, ?, ?, state, cursor_unix_nanos
		FROM schedules WHERE schedule_id = ?`, future.ScheduleID, encoded, digests.BriefSHA256,
		digests.DefinitionSHA256, definition.ScheduleID); err != nil {
		t.Fatal(err)
	}
	err = checkSchedulerDefinitions(t.Context(), schedules)
	if !errors.Is(err, host.ErrIncompatible) || !errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("unreadable definition was not a permanent readiness refusal: %v", err)
	}
	if err = database.QueryRow("SELECT request_json FROM schedules WHERE schedule_id = ?", future.ScheduleID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, data) {
		t.Fatal("readiness preflight rewrote a future definition")
	}
}

type schedulerReaderFault struct {
	contract.Store
	err error
}

func (s schedulerReaderFault) ListSchedules(context.Context, contract.SchedulePage) (contract.SchedulePageResult, error) {
	return contract.SchedulePageResult{}, s.err
}

func TestSchedulerReaderKeepsTransientFaultsRetryable(t *testing.T) {
	err := checkSchedulerDefinitions(t.Context(), schedulerReaderFault{err: task.ErrLockBusy})
	if !errors.Is(err, task.ErrLockBusy) || errors.Is(err, host.ErrIncompatible) {
		t.Fatalf("temporary contention was made permanent: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = checkSchedulerDefinitions(ctx, schedulerReaderFault{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
