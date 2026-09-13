package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Control records must fit the same serialized ceiling used by every reader.
// Measure canonical JSON, including escapes and its newline, before staging.
// Briefs and streamed payloads do not use this control-only boundary.
func marshalControlRecord(record any) ([]byte, error) {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return nil, err
	}
	if len(data) > task.MaxControlRecordSize {
		return nil, task.ErrControlRecordTooBig
	}
	return data, nil
}

// Matching preparation retries retain the stored incidental timestamp and bytes.
// Reusing an existing bounded record must not depend on the new timestamp's size.
// The admission-locked preparation still validates every fragment and guard.
func (s *Store) marshalPreparedMeta(id string, proposed *task.MetaRecord) ([]byte, error) {
	existing, err := s.readBytes(filepath.Join(s.Root, "tasks", id, "meta.json"), task.MaxControlRecordSize)
	if errors.Is(err, os.ErrNotExist) {
		return marshalControlRecord(proposed)
	}
	if err != nil {
		return nil, err
	}
	var saved task.MetaRecord
	if err = task.DecodeStrict(existing, &saved); err != nil {
		return nil, err
	}
	if err = task.ValidateMetaRecord(&saved); err != nil {
		return nil, err
	}
	comparison := *proposed
	comparison.CreatedAt = saved.CreatedAt
	if reflect.DeepEqual(saved, comparison) {
		return existing, nil
	}
	return marshalControlRecord(proposed)
}
