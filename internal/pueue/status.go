package pueue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type Job struct {
	ID        int64
	Label     *string
	State     State
	Group     string
	Succeeded bool
}

// Group records the bounded scheduling facts needed to verify an inspection
// queue. Neither a running group nor an ended job proves provider completion.
type Group struct {
	Status        string
	ParallelTasks uint64
}

// QueueSnapshot retains scheduling and successful worker-exit evidence without
// exposing queued command strings, process environments, or native error text.
type QueueSnapshot struct {
	Jobs   []Job
	Groups map[string]Group
}

// ParseStatus validates the exact serialized State/Task schema from pueue4.0.4.
// It does not return commands or task environment values in observations.
func ParseStatus(data []byte, version string) ([]Job, error) {
	snapshot, err := ParseQueueSnapshot(data, version)
	return snapshot.Jobs, err
}

// ParseQueueSnapshot validates the same pinned status schema as ParseStatus,
// retaining the group and worker-success facts used by native inspection.
func ParseQueueSnapshot(data []byte, version string) (QueueSnapshot, error) {
	if version != SupportedVersion {
		return QueueSnapshot{}, ErrBinding
	}
	if err := task.ValidateJSONStructure(data); err != nil {
		return QueueSnapshot{}, fmt.Errorf("%w: malformed status JSON", ErrUnknown)
	}
	root, err := statusObject(data, "tasks", "groups")
	if err != nil {
		return QueueSnapshot{}, err
	}
	groups, err := parseGroups(root["groups"])
	if err != nil {
		return QueueSnapshot{}, err
	}
	rows, err := rawObject(root["tasks"])
	if err != nil {
		return QueueSnapshot{}, err
	}
	jobs := make([]Job, 0, len(rows))
	for key, raw := range rows {
		id, err := parseID(key)
		if err != nil {
			return QueueSnapshot{}, err
		}
		job, err := parseJob(raw, id)
		if err != nil {
			return QueueSnapshot{}, err
		}
		jobs = append(jobs, job)
	}
	return QueueSnapshot{Jobs: jobs, Groups: groups}, nil
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10) != s {
		return 0, fmt.Errorf("%w: invalid numeric task ID", ErrUnknown)
	}
	return id, nil
}

func rawObject(raw []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%w: object required", ErrUnknown)
	}
	return obj, nil
}

func statusObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	obj, err := rawObject(raw)
	if err != nil {
		return nil, err
	}
	if len(obj) != len(names) {
		return nil, fmt.Errorf("%w: unexpected status fields", ErrUnknown)
	}
	for _, name := range names {
		if _, present := obj[name]; !present {
			return nil, fmt.Errorf("%w: missing status field", ErrUnknown)
		}
	}
	return obj, nil
}

func decodeScalar[T any](raw []byte) (T, error) {
	var v T
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return v, ErrUnknown
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("%w: invalid scalar", ErrUnknown)
	}
	return v, nil
}

func parseJob(raw []byte, id int64) (Job, error) {
	obj, err := statusObject(raw, "id", "created_at", "original_command", "command", "path", "envs", "group", "dependencies", "priority", "label", "status")
	if err != nil {
		return Job{}, err
	}
	actualID, err := decodeScalar[int64](obj["id"])
	if err != nil || id != actualID {
		return Job{}, fmt.Errorf("%w: numeric row identity mismatch", ErrUnknown)
	}
	if fieldsErr := validateJobFields(obj); fieldsErr != nil {
		return Job{}, fieldsErr
	}
	var label *string
	if !bytes.Equal(bytes.TrimSpace(obj["label"]), []byte("null")) {
		s, labelErr := decodeScalar[string](obj["label"])
		if labelErr != nil {
			return Job{}, labelErr
		}
		label = &s
	}
	state, err := parseJobState(obj["status"], 0)
	if err != nil {
		return Job{}, err
	}
	group, err := decodeScalar[string](obj["group"])
	if err != nil {
		return Job{}, err
	}
	succeeded, err := jobSucceeded(obj["status"], state)
	if err != nil {
		return Job{}, err
	}
	return Job{ID: id, Label: label, State: state, Group: group, Succeeded: succeeded}, nil
}

func jobSucceeded(raw []byte, state State) (bool, error) {
	if state != StateEnded {
		return false, nil
	}
	outer, err := rawObject(raw)
	if err != nil {
		return false, err
	}
	done, err := rawObject(outer["Done"])
	if err != nil {
		return false, err
	}
	// The state parser has already validated every result variant. Object
	// variants represent failure; only the decoded Success string qualifies.
	var result string
	return json.Unmarshal(done["result"], &result) == nil && result == "Success", nil
}

func validateJobFields(obj map[string]json.RawMessage) error {
	for _, key := range []string{"original_command", "command", "path"} {
		if _, err := decodeScalar[string](obj[key]); err != nil {
			return err
		}
	}
	if err := validateTime(obj["created_at"]); err != nil {
		return err
	}
	if _, err := decodeScalar[int32](obj["priority"]); err != nil {
		return err
	}
	if err := validateStringMap(obj["envs"]); err != nil {
		return err
	}
	dependencies, err := decodeScalar[[]int64](obj["dependencies"])
	if err != nil {
		return err
	}
	for _, id := range dependencies {
		if id < 0 {
			return ErrUnknown
		}
	}
	return nil
}

func validateStringMap(raw []byte) error {
	obj, err := rawObject(raw)
	if err != nil {
		return err
	}
	for _, value := range obj {
		if _, err := decodeScalar[string](value); err != nil {
			return err
		}
	}
	return nil
}

func parseGroups(raw []byte) (map[string]Group, error) {
	groups, err := rawObject(raw)
	if err != nil {
		return nil, err
	}
	parsed := make(map[string]Group, len(groups))
	for name, raw := range groups {
		obj, err := statusObject(raw, "status", "parallel_tasks")
		if err != nil {
			return nil, err
		}
		s, err := decodeScalar[string](obj["status"])
		if err != nil || (s != "Running" && s != "Paused" && s != "Reset") {
			return nil, ErrUnknown
		}
		parallel, err := decodeScalar[uint64](obj["parallel_tasks"])
		if err != nil {
			return nil, err
		}
		parsed[name] = Group{Status: s, ParallelTasks: parallel}
	}
	return parsed, nil
}

func validateTime(raw []byte) error {
	s, err := decodeScalar[string](raw)
	if err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return fmt.Errorf("%w: invalid timestamp", ErrUnknown)
	}
	return nil
}

func parseJobState(raw []byte, depth int) (State, error) {
	if depth > 32 {
		return StateUnknown, ErrUnknown
	}
	obj, err := rawObject(raw)
	if err != nil || len(obj) != 1 {
		return StateUnknown, ErrUnknown
	}
	for variant, value := range obj {
		return parseStateVariant(variant, value, depth)
	}
	return StateUnknown, ErrUnknown
}

func parseStateVariant(variant string, raw []byte, depth int) (State, error) {
	switch variant {
	case "Queued":
		return timestampState(raw, StateQueued, "enqueued_at")
	case "Running", "Paused":
		return timestampState(raw, StateRunning, "enqueued_at", "start")
	case "Stashed":
		return stashedState(raw)
	case "Done":
		return doneState(raw)
	case "Locked":
		obj, err := statusObject(raw, "previous_status")
		if err != nil {
			return StateUnknown, err
		}
		if _, err := parseJobState(obj["previous_status"], depth+1); err != nil {
			return StateUnknown, err
		}
		return StateUnknown, nil
	default:
		return StateUnknown, fmt.Errorf("%w: unsupported task state", ErrUnknown)
	}
}

func timestampState(raw []byte, state State, fields ...string) (State, error) {
	obj, err := statusObject(raw, fields...)
	if err != nil {
		return StateUnknown, err
	}
	for _, value := range obj {
		if err := validateTime(value); err != nil {
			return StateUnknown, err
		}
	}
	return state, nil
}

func stashedState(raw []byte) (State, error) {
	obj, err := statusObject(raw, "enqueue_at")
	if err != nil {
		return StateUnknown, err
	}
	if !bytes.Equal(bytes.TrimSpace(obj["enqueue_at"]), []byte("null")) {
		if err := validateTime(obj["enqueue_at"]); err != nil {
			return StateUnknown, err
		}
	}
	return StateQueued, nil
}

func doneState(raw []byte) (State, error) {
	obj, err := statusObject(raw, "enqueued_at", "start", "end", "result")
	if err != nil {
		return StateUnknown, err
	}
	for _, key := range []string{"enqueued_at", "start", "end"} {
		if err := validateTime(obj[key]); err != nil {
			return StateUnknown, err
		}
	}
	if err := validateTaskResult(obj["result"]); err != nil {
		return StateUnknown, err
	}
	return StateEnded, nil
}

func validateTaskResult(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "Success", "Killed", "Errored", "DependencyFailed":
			return nil
		default:
			return ErrUnknown
		}
	}
	obj, err := rawObject(raw)
	if err != nil || len(obj) != 1 {
		return ErrUnknown
	}
	for key, value := range obj {
		switch key {
		case "Failed":
			_, err := decodeScalar[int32](value)
			return err
		case "FailedToSpawn":
			_, err := decodeScalar[string](value)
			return err
		default:
			return ErrUnknown
		}
	}
	return ErrUnknown
}
