package control

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
)

const statusTestTime = "2026-09-13T20:00:00.123456789Z"

func statusTestPayload(t *testing.T, jobs map[string]any, groups map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"tasks": jobs, "groups": groups})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func statusTestJob(id int64, label any, state map[string]any) map[string]any {
	return map[string]any{
		"id": id, "created_at": statusTestTime,
		"original_command": "runner --root /tmp/task", "command": "runner --root /tmp/task",
		"path": "/tmp", "envs": map[string]string{}, "group": "default",
		"dependencies": []int64{}, "priority": int32(0), "label": label, "status": state,
	}
}

func statusStreamTestRunningState() map[string]any {
	return map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
}

func statusStreamTestDoneState(result any) map[string]any {
	return map[string]any{"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": result}}
}

func statusStreamTestLargeQueue(t *testing.T, ended int) ([]byte, QueueSnapshot) {
	t.Helper()
	jobs := make(map[string]any, 200)
	want := QueueSnapshot{Groups: map[string]Group{"default": {Status: "Running", ParallelTasks: 200}}}
	path := strings.Repeat("p", 6000)
	for id := range int64(200) {
		label := "job-" + strconv.FormatInt(id, 10)
		state := statusStreamTestRunningState()
		projected := Job{ID: id, Label: &label, State: StateRunning, Group: "default"}
		if id < int64(ended) {
			state = statusStreamTestDoneState("Success")
			projected.State = StateEnded
			projected.Succeeded = true
		}
		projected = protocol.WithOriginalCommand(projected, "runner --root /tmp/task")
		job := statusTestJob(id, label, state)
		job["envs"] = map[string]string{"PATH": path}
		jobs[strconv.FormatInt(id, 10)] = job
		want.Jobs = append(want.Jobs, projected)
	}
	return statusTestPayload(t, jobs, map[string]any{
		"default": map[string]any{"status": "Running", "parallel_tasks": 200},
	}), want
}
