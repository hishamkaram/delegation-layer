package fakesupervisor

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// BuildStatusFixture returns a small raw pueue 4.0.4 status document. The
// fake client copies configured status bytes without parsing or repairing them;
// this helper is only for positive test fixtures.
func BuildStatusFixture(jobs []StatusJob) ([]byte, error) {
	tasks := make(map[string]statusFixtureJob, len(jobs))
	for _, job := range jobs {
		if job.ID < 0 || job.Label == "" {
			return nil, fmt.Errorf("%w: invalid status fixture job", ErrInvalidConfig)
		}
		state, err := statusFixtureState(job.State)
		if err != nil {
			return nil, err
		}
		id := strconv.FormatInt(job.ID, 10)
		if _, exists := tasks[id]; exists {
			return nil, fmt.Errorf("%w: duplicate status fixture ID", ErrInvalidConfig)
		}
		tasks[id] = statusFixtureJob{
			ID:              job.ID,
			CreatedAt:       fixtureTime,
			OriginalCommand: "supervisor-runner --root /tmp/task",
			Command:         "supervisor-runner --root /tmp/task",
			Path:            "/tmp",
			Envs:            map[string]string{},
			Group:           "default",
			Dependencies:    []int64{},
			Priority:        0,
			Label:           job.Label,
			Status:          state,
		}
	}
	return json.Marshal(statusFixtureEnvelope{Tasks: tasks, Groups: map[string]any{}})
}

const fixtureTime = "2026-09-13T20:00:00.123456789Z"

type statusFixtureEnvelope struct {
	Tasks  map[string]statusFixtureJob `json:"tasks"`
	Groups map[string]any              `json:"groups"`
}

type statusFixtureJob struct {
	ID              int64             `json:"id"`
	CreatedAt       string            `json:"created_at"`
	OriginalCommand string            `json:"original_command"`
	Command         string            `json:"command"`
	Path            string            `json:"path"`
	Envs            map[string]string `json:"envs"`
	Group           string            `json:"group"`
	Dependencies    []int64           `json:"dependencies"`
	Priority        int32             `json:"priority"`
	Label           string            `json:"label"`
	Status          map[string]any    `json:"status"`
}

func statusFixtureState(state string) (map[string]any, error) {
	switch state {
	case "queued":
		return map[string]any{"Queued": map[string]any{"enqueued_at": fixtureTime}}, nil
	case "running":
		return map[string]any{"Running": map[string]any{"enqueued_at": fixtureTime, "start": fixtureTime}}, nil
	case "ended":
		return map[string]any{"Done": map[string]any{"enqueued_at": fixtureTime, "start": fixtureTime, "end": fixtureTime, "result": "Success"}}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported status fixture state", ErrInvalidConfig)
	}
}
