package pueue

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestParseQueueSnapshotStreamLargeQueues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ended int
	}{
		{name: "200 running jobs"},
		{name: "199 ended and one running", ended: 199},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, want := statusStreamTestLargeQueue(t, tc.ended)
			if len(data) <= 1<<20 {
				t.Fatalf("large queue fixture has only %d bytes", len(data))
			}
			for _, reader := range []struct {
				name string
				read io.Reader
			}{
				{name: "buffered", read: bytes.NewReader(data)},
				{name: "short reads", read: iotest.HalfReader(bytes.NewReader(data))},
			} {
				t.Run(reader.name, func(t *testing.T) {
					got, err := parseQueueSnapshotStream(reader.read, FixtureVersion)
					if err != nil {
						t.Fatalf("aggregate queue of %d bytes was rejected: %v", len(data), err)
					}
					statusStreamTestSnapshot(t, got, want)
				})
			}
		})
	}
}

func statusStreamTestLargeQueue(t *testing.T, ended int) ([]byte, QueueSnapshot) {
	t.Helper()
	jobs := make(map[string]any, 200)
	want := QueueSnapshot{Groups: map[string]Group{"default": {Status: "Running", ParallelTasks: 200}}}
	path := strings.Repeat("p", 6000)
	for id := range int64(200) {
		label := "job-" + strconv.FormatInt(id, 10)
		state := statusStreamTestRunningState()
		projected := Job{ID: id, Label: &label, State: StateRunning, Group: "default", originalCommand: "runner --root /tmp/task"}
		if id < int64(ended) {
			state = statusStreamTestDoneState("Success")
			projected.State = StateEnded
			projected.Succeeded = true
		}
		job := statusTestJob(id, label, state)
		job["envs"] = map[string]string{"PATH": path}
		jobs[strconv.FormatInt(id, 10)] = job
		want.Jobs = append(want.Jobs, projected)
	}
	data := statusTestPayload(t, jobs, map[string]any{
		"default": map[string]any{"status": "Running", "parallel_tasks": 200},
	})
	return data, want
}

func TestParseQueueSnapshotStreamObservedVersions(t *testing.T) {
	job := statusTestJob(7, nil, map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})
	data := statusTestPayload(t, map[string]any{"7": job}, map[string]any{
		"default": map[string]any{"status": "Running", "parallel_tasks": 2},
		"paused":  map[string]any{"status": "Paused", "parallel_tasks": 0},
		"reset":   map[string]any{"status": "Reset", "parallel_tasks": 3},
	})
	want := QueueSnapshot{
		Jobs: []Job{{ID: 7, State: StateQueued, Group: "default", originalCommand: "runner --root /tmp/task"}},
		Groups: map[string]Group{
			"default": {Status: "Running", ParallelTasks: 2},
			"paused":  {Status: "Paused", ParallelTasks: 0},
			"reset":   {Status: "Reset", ParallelTasks: 3},
		},
	}
	for _, version := range []string{FixtureVersion, "4.0.3", "pueue future"} {
		t.Run(version, func(t *testing.T) {
			got, err := parseQueueSnapshotStream(iotest.DataErrReader(bytes.NewReader(data)), version)
			if err != nil {
				t.Fatal(err)
			}
			statusStreamTestSnapshot(t, got, want)
		})
	}
	t.Run("missing version", func(t *testing.T) {
		got, err := parseQueueSnapshotStream(bytes.NewReader(data), "")
		if !errors.Is(err, ErrBinding) {
			t.Fatalf("missing version error = %v, want ErrBinding", err)
		}
		statusStreamTestNoPartialSnapshot(t, got)
	})
}

func TestParseQueueSnapshotStreamAcceptsEmptyQueueAndWhitespace(t *testing.T) {
	data := " \n\t" + string(statusTestPayload(t, map[string]any{}, map[string]any{})) + "\r\n "
	got, err := parseQueueSnapshotStream(iotest.OneByteReader(strings.NewReader(data)), FixtureVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Jobs) != 0 || len(got.Groups) != 0 {
		t.Fatalf("empty queue returned metadata: %+v", got)
	}
}

func TestParseQueueSnapshotStreamAcceptsIndependentBoundedRecords(t *testing.T) {
	// Each record fits the bound; together the task, group, and additive values
	// require the decoder to advance its read window several times.
	padding := strings.Repeat("p", MaxControlBytes-1024)
	job := statusTestJob(7, nil, map[string]any{
		"Queued": map[string]any{"enqueued_at": statusTestTime, "future_state_field": true},
	})
	job["envs"] = map[string]string{"PATH": padding}
	job["future_job_field"] = []any{nil, true, map[string]any{"value": 1}}
	data := statusStreamTestJSON(t, map[string]any{
		"tasks": map[string]any{"7": job},
		"groups": map[string]any{
			"default": map[string]any{"status": "Running", "parallel_tasks": 2, "future_group_field": padding},
		},
		"future_object": map[string]any{"data": padding},
		"future_array":  []any{padding, map[string]any{"value": nil}},
		"future_string": padding,
		"future_number": 123,
		"future_bool":   true,
		"future_null":   nil,
	})
	if len(data) <= MaxControlBytes {
		t.Fatalf("aggregate fixture has only %d bytes", len(data))
	}
	got, err := parseQueueSnapshotStream(bytes.NewReader(data), FixtureVersion)
	if err != nil {
		t.Fatal(err)
	}
	statusStreamTestSnapshot(t, got, QueueSnapshot{
		Jobs:   []Job{{ID: 7, State: StateQueued, Group: "default", originalCommand: "runner --root /tmp/task"}},
		Groups: map[string]Group{"default": {Status: "Running", ParallelTasks: 2}},
	})
}

func TestParseQueueSnapshotStreamRejectsDuplicateKeys(t *testing.T) {
	job := statusTestJob(7, "label", statusStreamTestRunningState())
	row := string(statusStreamTestJSON(t, job))
	group := `{"status":"Running","parallel_tasks":1}`
	valid := string(statusTestPayload(t, map[string]any{"7": job}, map[string]any{}))
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "tasks root", data: `{"tasks":{},"tasks":{},"groups":{}}`},
		{name: "groups root", data: `{"tasks":{},"groups":{},"groups":{}}`},
		{name: "root case alias", data: `{"tasks":{},"TASKS":{},"groups":{}}`},
		{name: "root unicode alias", data: `{"tasks":{},"taſks":{},"groups":{}}`},
		{name: "root escaped alias", data: `{"tasks":{},"ta\u0073ks":{},"groups":{}}`},
		{name: "task ID", data: `{"tasks":{"7":` + row + `,"7":` + row + `},"groups":{}}`},
		{name: "escaped task ID", data: `{"tasks":{"7":` + row + `,"\u0037":` + row + `},"groups":{}}`},
		{name: "row field", data: strings.Replace(valid, `"label":"label"`, `"label":"label","label":"other"`, 1)},
		{name: "row case alias", data: strings.Replace(valid, `"label":"label"`, `"label":"label","LABEL":"other"`, 1)},
		{name: "environment key", data: strings.Replace(valid, `"envs":{}`, `"envs":{"PATH":"a","PATH":"b"}`, 1)},
		{name: "environment case alias", data: strings.Replace(valid, `"envs":{}`, `"envs":{"PATH":"a","path":"b"}`, 1)},
		{name: "environment unicode alias", data: strings.Replace(valid, `"envs":{}`, `"envs":{"K":"a","K":"b"}`, 1)},
		{name: "group name", data: `{"tasks":{},"groups":{"default":` + group + `,"default":` + group + `}}`},
		{name: "group case alias", data: `{"tasks":{},"groups":{"default":` + group + `,"DEFAULT":` + group + `}}`},
		{name: "group unicode alias", data: `{"tasks":{},"groups":{"K":` + group + `,"K":` + group + `}}`},
		{name: "group field alias", data: `{"tasks":{},"groups":{"default":{"status":"Running","STATUS":"Paused","parallel_tasks":1}}}`},
		{name: "additive root field", data: `{"tasks":{},"groups":{},"future":1,"FUTURE":2}`},
		{name: "additive object", data: `{"tasks":{},"groups":{},"future":{"a":1,"a":2}}`},
		{name: "additive array", data: `{"tasks":{},"groups":{},"future":[{"a":1,"A":2}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusStreamTestRejects(t, iotest.OneByteReader(strings.NewReader(tc.data)), task.ErrDuplicateKey)
		})
	}
}

func TestParseQueueSnapshotStreamRejectsMalformedStatus(t *testing.T) {
	row := string(statusStreamTestJSON(t, statusTestJob(7, nil, statusStreamTestRunningState())))
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "empty"},
		{name: "whitespace", data: " \n\t"},
		{name: "null root", data: `null`},
		{name: "array root", data: `[]`},
		{name: "scalar root", data: `true`},
		{name: "missing tasks", data: `{"groups":{}}`},
		{name: "missing groups", data: `{"tasks":{}}`},
		{name: "wrong field case", data: `{"TASKS":{},"groups":{}}`},
		{name: "null tasks", data: `{"tasks":null,"groups":{}}`},
		{name: "null groups", data: `{"tasks":{},"groups":null}`},
		{name: "array tasks", data: `{"tasks":[],"groups":{}}`},
		{name: "array groups", data: `{"tasks":{},"groups":[]}`},
		{name: "null row", data: `{"tasks":{"7":null},"groups":{}}`},
		{name: "null group", data: `{"tasks":{},"groups":{"default":null}}`},
		{name: "incomplete row", data: `{"tasks":{"7":{"id":7}},"groups":{}}`},
		{name: "incomplete group", data: `{"tasks":{},"groups":{"default":{"status":"Running"}}}`},
		{name: "unknown group state", data: `{"tasks":{},"groups":{"default":{"status":"Unknown","parallel_tasks":1}}}`},
		{name: "truncated root", data: `{"tasks":{},"groups":{}`},
		{name: "truncated row", data: `{"tasks":{"7":` + row[:len(row)-1]},
		{name: "trailing comma", data: `{"tasks":{},"groups":{},}`},
		{name: "malformed additive value", data: `{"tasks":{},"groups":{},"future":[1,]}`},
		{name: "later invalid row", data: `{"tasks":{"7":` + row + `,"8":null},"groups":{}}`},
		{name: "later invalid group", data: `{"tasks":{"7":` + row + `},"groups":{"default":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusStreamTestRejects(t, strings.NewReader(tc.data))
		})
	}
	for _, id := range []string{"-1", "07", "+7", "9223372036854775808", "8"} {
		t.Run("invalid task ID "+id, func(t *testing.T) {
			data := statusTestPayload(t, map[string]any{id: json.RawMessage(row)}, map[string]any{})
			statusStreamTestRejects(t, bytes.NewReader(data))
		})
	}
}

func TestParseQueueSnapshotStreamValidatesEndedRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{name: "null environment", field: "envs", value: nil},
		{name: "nonstring environment", field: "envs", value: map[string]any{"PATH": 1}},
		{name: "null environment value", field: "envs", value: map[string]any{"PATH": nil}},
		{name: "null command", field: "command", value: nil},
		{name: "invalid creation time", field: "created_at", value: "later"},
		{name: "priority overflow", field: "priority", value: int64(2147483648)},
		{name: "negative dependency", field: "dependencies", value: []int64{-1}},
		{name: "null state", field: "status", value: nil},
		{name: "invalid result", field: "status", value: statusStreamTestDoneState("Unknown")},
		{name: "ambiguous state", field: "status", value: map[string]any{
			"Done":    map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success"},
			"Aborted": map[string]any{},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ended := statusTestJob(1, nil, statusStreamTestDoneState("Success"))
			ended[tc.field] = tc.value
			data := statusTestPayload(t, map[string]any{
				"0": statusTestJob(0, "running", statusStreamTestRunningState()),
				"1": ended,
			}, map[string]any{})
			statusStreamTestRejects(t, bytes.NewReader(data))
		})
	}
}

func TestParseQueueSnapshotStreamRejectsTrailingJSON(t *testing.T) {
	job := statusTestJob(7, nil, statusStreamTestRunningState())
	valid := string(statusTestPayload(t, map[string]any{"7": job}, map[string]any{}))
	for _, suffix := range []string{`{}`, `[]`, `null`, `true`, `0`, `"text"`, `garbage`, `}`} {
		t.Run(suffix, func(t *testing.T) {
			statusStreamTestRejects(t, strings.NewReader(valid+" \n"+suffix), task.ErrTrailingJSON)
		})
	}
}

func TestParseQueueSnapshotStreamRejectsOversizedRecords(t *testing.T) {
	padding := strings.Repeat("p", MaxControlBytes+1)
	job := statusTestJob(7, nil, statusStreamTestRunningState())
	job["envs"] = map[string]string{"PATH": padding}
	validRow := string(statusStreamTestJSON(t, statusTestJob(7, nil, statusStreamTestRunningState())))
	whitespace := strings.Repeat(" ", MaxControlBytes+1)
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "task", data: string(statusTestPayload(t, map[string]any{"7": job}, map[string]any{}))},
		{name: "group", data: string(statusTestPayload(t, map[string]any{}, map[string]any{
			"default": map[string]any{"status": "Running", "parallel_tasks": 1, "future": padding},
		}))},
		{name: "additive scalar", data: `{"tasks":{},"groups":{},"future":"` + padding + `"}`},
		{name: "additive object", data: `{"tasks":{},"groups":{},"future":{"data":"` + padding + `"}}`},
		{name: "additive array", data: `{"tasks":{},"groups":{},"future":["` + padding + `"]}`},
		{name: "task leading whitespace", data: `{"tasks":{"7":` + whitespace + validRow + `},"groups":{}}`},
		{name: "task trailing whitespace", data: `{"tasks":{"7":` + validRow + whitespace + `},"groups":{}}`},
		{name: "root leading whitespace", data: whitespace + `{"tasks":{},"groups":{}}`},
		{name: "root trailing whitespace", data: `{"tasks":{},"groups":{}}` + whitespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusStreamTestRejects(t, strings.NewReader(tc.data), ErrControlLimit)
		})
	}
}

func TestParseQueueSnapshotStreamPreservesReaderErrors(t *testing.T) {
	data := statusTestPayload(t, map[string]any{"7": statusTestJob(7, nil, statusStreamTestRunningState())}, map[string]any{})
	readErr := errors.New("injected status read failure")
	for _, offset := range []int{0, len(data) / 2, len(data)} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			reader := io.MultiReader(bytes.NewReader(data[:offset]), iotest.ErrReader(readErr))
			statusStreamTestRejects(t, reader, readErr)
		})
	}
}

func statusStreamTestRunningState() map[string]any {
	return map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
}

func statusStreamTestDoneState(result any) map[string]any {
	return map[string]any{"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": result}}
}

func statusStreamTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func statusStreamTestSnapshot(t *testing.T, got, want QueueSnapshot) {
	t.Helper()
	slices.SortFunc(got.Jobs, func(left, right Job) int { return cmp.Compare(left.ID, right.ID) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func statusStreamTestRejects(t *testing.T, reader io.Reader, wantErrors ...error) {
	t.Helper()
	got, err := parseQueueSnapshotStream(reader, FixtureVersion)
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("invalid status error = %v, want ErrUnknown", err)
	}
	for _, want := range wantErrors {
		if !errors.Is(err, want) {
			t.Fatalf("invalid status error = %v, want %v", err, want)
		}
	}
	statusStreamTestNoPartialSnapshot(t, got)
}

func statusStreamTestNoPartialSnapshot(t *testing.T, got QueueSnapshot) {
	t.Helper()
	if len(got.Jobs) != 0 || len(got.Groups) != 0 {
		t.Fatalf("invalid status returned a partial snapshot: %+v", got)
	}
}

func TestParseQueueSnapshotStreamExactValueLimit(t *testing.T) {
	row := string(statusStreamTestJSON(t, statusTestJob(7, nil, statusStreamTestRunningState())))
	for _, kind := range []string{"task", "group"} {
		for _, separator := range []string{":", ": ", ":\n\t"} {
			t.Run(kind+separator, func(t *testing.T) {
				data := statusStreamExactLimitFixture(t, row, kind, separator)
				for _, reader := range []io.Reader{strings.NewReader(data), iotest.HalfReader(strings.NewReader(data))} {
					if _, err := parseQueueSnapshotStream(reader, FixtureVersion); err != nil {
						t.Fatalf("exact-limit value rejected: %v", err)
					}
				}
			})
		}
	}
}

func statusStreamExactLimitFixture(t *testing.T, row, kind, separator string) string {
	t.Helper()
	value := `{"status":"Running","parallel_tasks":0}`
	if kind == "task" {
		value = row
	}
	value = value[:len(value)-1] + `,"padding":""}`
	value = value[:len(value)-2] + strings.Repeat("x", MaxControlBytes-len(value)) + value[len(value)-2:]
	if len(value) != MaxControlBytes {
		t.Fatal("invalid exact-limit fixture")
	}
	data := `{"tasks":{},"groups":{"default"` + separator + value + `}}`
	if kind == "task" {
		data = `{"tasks":{"7"` + separator + value + `},"groups":{}}`
	}
	return data
}

func TestParseQueueSnapshotStreamBoundsOversizedValueConsumption(t *testing.T) {
	prefix := `{"tasks":{},"groups":{},"future": `
	data := prefix + `"` + strings.Repeat("x", 4*MaxControlBytes) + `"}`
	reader := strings.NewReader(data)
	statusStreamTestRejects(t, reader, ErrControlLimit)
	if consumed := len(data) - reader.Len(); consumed > len(prefix)+2*MaxControlBytes+2 {
		t.Fatalf("oversized value read %d bytes before rejection", consumed)
	}
}
