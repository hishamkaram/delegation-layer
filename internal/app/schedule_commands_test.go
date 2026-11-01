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
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const scheduleTestID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func scheduleFixture(t *testing.T) (string, contract.Store, contract.Definition) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	state, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	store, err := openScheduleForCreate(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	definition := contract.Definition{DefinitionHeader: contract.DefinitionHeader{
		ScheduleID: scheduleTestID, CreatedAt: time.Now().UTC().Add(-time.Hour), Cron: "0 9 * * *", Timezone: "UTC",
		Request: contract.Request{Auto: true, CanonicalCwd: t.TempDir()},
	}, Brief: []byte("private brief content")}
	if err := store.CreateSchedule(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	return root, store, definition
}

func runScheduleTest(t *testing.T, root string, args ...string) (Response, int, string) {
	t.Helper()
	argv := append([]string{"--root", root, "schedule"}, args...)
	argv = append(argv, "--json")
	var stdout, stderr bytes.Buffer
	code := Run(argv, &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v; stderr=%s", stdout.String(), err, stderr.String())
	}
	return response, code, stdout.String()
}

func TestScheduleReadsDoNotInitialize(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	list, code, _ := runScheduleTest(t, root, "list")
	if code != 0 || list.Schedules == nil || len(*list.Schedules) != 0 {
		t.Fatalf("list code=%d response=%+v", code, list)
	}
	for _, action := range []string{"show", "pause", "resume", "delete"} {
		response, code, _ := runScheduleTest(t, root, action, scheduleTestID)
		if code != 1 || response.Schedule == nil || response.Schedule.ID != scheduleTestID {
			t.Fatalf("%s code=%d response=%+v", action, code, response)
		}
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read created root or failed to inspect it: %v", err)
	}
}

func TestScheduleReadsDoNotInitializeExistingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	list, code, _ := runScheduleTest(t, root, "list")
	if code != 0 || list.Schedules == nil || len(*list.Schedules) != 0 {
		t.Fatalf("list code=%d response=%+v", code, list)
	}
	if _, err := os.Lstat(filepath.Join(root, ".scheduler")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read created scheduler directory or failed to inspect it: %v", err)
	}
}

func TestScheduleCreateOpenRefusesCorruptStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	directory := filepath.Join(root, ".scheduler")
	if mkdirErr := os.Mkdir(directory, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	path := filepath.Join(directory, "schedules.sqlite")
	contents := []byte("not a database")
	if writeErr := os.WriteFile(path, contents, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	opened, err := openScheduleForCreate(t.Context(), state)
	if opened != nil || !errors.Is(err, task.ErrUnsupportedSchema) {
		t.Fatalf("opened=%v err=%v", opened, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, contents) {
		t.Fatalf("database changed: %q err=%v", after, err)
	}
}

func TestScheduleReadAndStatePreserveOccurrence(t *testing.T) {
	root, store, definition := scheduleFixture(t)
	due := definition.CreatedAt.Add(time.Minute)
	claim := contract.Claim{ScheduleID: scheduleTestID, ExpectedCursor: definition.CreatedAt, Due: due, TaskID: strings.Repeat("b", 32), ArmedAt: due}
	if _, err := store.ClaimAndArm(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	show, code, output := runScheduleTest(t, root, "show", scheduleTestID)
	if code != 0 || show.Schedule == nil || show.Schedule.Request == nil || show.Schedule.BriefLength != len(definition.Brief) || strings.Contains(output, string(definition.Brief)) {
		t.Fatalf("show code=%d response=%+v", code, show)
	}
	list, code, _ := runScheduleTest(t, root, "list")
	if code != 0 || list.Schedules == nil || len(*list.Schedules) != 1 || (*list.Schedules)[0].ID != scheduleTestID {
		t.Fatalf("list code=%d response=%+v", code, list)
	}
	assertScheduleStateSequence(t, root)
	assertScheduleOccurrenceHistory(t, store, claim)
	response, code, _ := runScheduleTest(t, root, "resume", scheduleTestID)
	if code != 2 || response.Error == "" {
		t.Fatalf("deleted resume code=%d response=%+v", code, response)
	}
}

func assertScheduleStateSequence(t *testing.T, root string) {
	t.Helper()
	for _, action := range []string{"pause", "pause", "resume", "delete", "delete"} {
		response, code, _ := runScheduleTest(t, root, action, scheduleTestID)
		wantCode := 0
		if action == "resume" {
			wantCode = 3
		}
		if code != wantCode || response.Schedule == nil || response.Schedule.ID != scheduleTestID {
			t.Fatalf("%s code=%d response=%+v", action, code, response)
		}
	}
}

func assertScheduleOccurrenceHistory(t *testing.T, store contract.Store, claim contract.Claim) {
	t.Helper()
	occurrence, err := store.GetOccurrence(t.Context(), claim.TaskID)
	if err != nil || occurrence.Release != nil {
		t.Fatalf("active occurrence changed: %+v err=%v", occurrence, err)
	}
	history, err := store.History(t.Context(), contract.HistoryPage{ScheduleID: scheduleTestID, Limit: 10})
	if err != nil || len(history.Entries) != 2 {
		t.Fatalf("history entries=%d err=%v", len(history.Entries), err)
	}
}

func TestScheduleResumeBehindCursor(t *testing.T) {
	root, store, definition := scheduleFixture(t)
	future := time.Now().UTC().Add(time.Hour)
	claim := contract.Claim{ScheduleID: scheduleTestID, ExpectedCursor: definition.CreatedAt, Due: future, TaskID: strings.Repeat("b", 32), ArmedAt: time.Now().UTC()}
	if _, err := store.ClaimAndArm(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if _, code, _ := runScheduleTest(t, root, "pause", scheduleTestID); code != 0 {
		t.Fatalf("pause code=%d", code)
	}
	response, code, _ := runScheduleTest(t, root, "resume", scheduleTestID)
	if code != 3 || !response.Schedule.Saved || len(response.ScheduleWarnings) != 1 || response.ScheduleWarnings[0].Code != "clock_behind_cursor" {
		t.Fatalf("resume code=%d response=%+v", code, response)
	}
	item, err := store.GetSchedule(t.Context(), scheduleTestID)
	if err != nil || !item.Cursor.Equal(future) {
		t.Fatalf("cursor=%v err=%v", item.Cursor, err)
	}
}

func TestScheduleMissingClassifierKeepsJoinedFault(t *testing.T) {
	if !onlyNotExist(errors.Join(fs.ErrNotExist, fs.ErrNotExist)) || onlyNotExist(errors.Join(fs.ErrNotExist, task.ErrInvariantFault)) {
		t.Fatal("absence classifier discarded a joined fault")
	}
}

func TestScheduleLargeShowAndBoundedList(t *testing.T) {
	root, store, definition := scheduleFixture(t)
	definition.ScheduleID = strings.Repeat("c", 32)
	encoded, err := contract.MarshalRequest(definition.Request)
	if err != nil {
		t.Fatal(err)
	}
	definition.Request.Config.Model = strings.Repeat("x", task.MaxControlRecordSize-len(encoded)-20)
	if err := store.CreateSchedule(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	show, code, output := runScheduleTest(t, root, "show", definition.ScheduleID)
	if code != 0 || len(output) <= MaxJSONResponseBytes || show.Schedule == nil || show.Schedule.Request.Config.Model != definition.Request.Config.Model {
		t.Fatalf("show code=%d bytes=%d", code, len(output))
	}
	list, code, output := runScheduleTest(t, root, "list")
	if code != 0 || list.Schedules == nil || len(*list.Schedules) != 2 || len(output) > 4096 || strings.Contains(output, definition.Request.Config.Model) {
		t.Fatalf("list code=%d bytes=%d", code, len(output))
	}
}

type scheduleFaultStore struct {
	contract.Store
	writeErr error
	readErr  error
	reads    int
}

func (s *scheduleFaultStore) ChangeState(ctx context.Context, change contract.StateChange) error {
	if err := s.Store.ChangeState(ctx, change); err != nil {
		return err
	}
	return s.writeErr
}

func (s *scheduleFaultStore) GetSchedule(ctx context.Context, id string) (contract.Schedule, error) {
	s.reads++
	if s.reads > 1 && s.readErr != nil {
		return contract.Schedule{}, s.readErr
	}
	return s.Store.GetSchedule(ctx, id)
}

func TestScheduleUncertainWriteReportsIDAndJoinedFault(t *testing.T) {
	_, store, _ := scheduleFixture(t)
	writeFault, readFault := errors.New("write cleanup fault"), errors.New("readback fault")
	faults := &scheduleFaultStore{Store: store, writeErr: writeFault, readErr: readFault}
	a := Arguments{ScheduleAction: "pause", ScheduleID: scheduleTestID}
	result := changeScheduleState(t.Context(), faults, a, newResponse("schedule"), time.Now().UTC())
	if result.code != 1 || !errors.Is(result.err, writeFault) || !errors.Is(result.err, readFault) || faults.reads != 2 || result.response.Schedule.ID != scheduleTestID || result.response.Schedule.NextAction != "inspect_schedule" {
		t.Fatalf("result=%+v reads=%d", result, faults.reads)
	}
	item, err := store.GetSchedule(t.Context(), scheduleTestID)
	if err != nil || item.State != contract.StatePaused {
		t.Fatalf("committed state=%s err=%v", item.State, err)
	}
}

func TestScheduleCreateReadbackUsesCapturedDefinition(t *testing.T) {
	_, store, definition := scheduleFixture(t)
	match, err := readBackScheduleDefinition(t.Context(), store, definition)
	if err != nil || !match {
		t.Fatalf("matching definition: match=%t err=%v", match, err)
	}
	definition.CreatedAt = definition.CreatedAt.Add(time.Nanosecond)
	match, err = readBackScheduleDefinition(t.Context(), store, definition)
	if match || !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("different identity: match=%t err=%v", match, err)
	}
}

type scheduleBarrierStore struct {
	contract.Store
	entered chan<- struct{}
	release <-chan struct{}
}

func (s scheduleBarrierStore) ChangeState(ctx context.Context, change contract.StateChange) error {
	s.entered <- struct{}{}
	select {
	case <-s.release:
		return s.Store.ChangeState(ctx, change)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestScheduleConcurrentPauseUsesStoreCAS(t *testing.T) {
	root, first, _ := scheduleFixture(t)
	state, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	second, err := scheduler.Open(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := second.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	results := make(chan commandResult, 2)
	for _, store := range []contract.Store{first, second} {
		go func() {
			barrier := scheduleBarrierStore{Store: store, entered: entered, release: release}
			results <- changeScheduleState(ctx, barrier, Arguments{ScheduleAction: "pause", ScheduleID: scheduleTestID}, newResponse("schedule"), time.Now().UTC())
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			for range 2 {
				<-results
			}
			t.Fatal("writers did not reach the store boundary")
		}
	}
	close(release)
	collected := [2]commandResult{<-results, <-results}
	assertConcurrentPauseResults(t, first, collected)
}

func assertConcurrentPauseResults(t *testing.T, store contract.Store, collected [2]commandResult) {
	t.Helper()
	successes := 0
	for _, result := range collected {
		if result.code == 0 {
			successes++
		} else if result.code != 1 || result.response.Schedule.ID != scheduleTestID {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	item, err := store.GetSchedule(t.Context(), scheduleTestID)
	if err != nil || item.State != contract.StatePaused || successes != 1 {
		t.Fatalf("successes=%d state=%s err=%v", successes, item.State, err)
	}
}

func TestScheduleOutputFailureReportsKnownID(t *testing.T) {
	root, _, _ := scheduleFixture(t)
	var stderr bytes.Buffer
	code := Run([]string{"--root", root, "schedule", "pause", scheduleTestID, "--json"}, errorWriter{err: errors.New("writer failed")}, &stderr, Dependencies{})
	if code != 1 || !strings.Contains(stderr.String(), scheduleTestID) || !strings.Contains(stderr.String(), "schedule show") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
