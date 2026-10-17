package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// statusRecordReader bounds decoder lookahead to one record and its syntax
// allowance. Resetting the window never retains prior task environments.
type statusRecordReader struct {
	reader   io.Reader
	position int64
	limit    int64
}

func (r *statusRecordReader) Read(p []byte) (int, error) {
	remaining := r.limit - r.position
	if remaining <= 0 {
		return 0, ErrControlLimit
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.position += int64(n)
	return n, err
}

type statusStream struct {
	reader  *statusRecordReader
	decoder *json.Decoder
}

func newStatusStream(reader io.Reader) *statusStream {
	bounded := &statusRecordReader{reader: reader, limit: MaxControlBytes + 1}
	return &statusStream{reader: bounded, decoder: json.NewDecoder(bounded)}
}

func (s *statusStream) window() { s.reader.limit = s.decoder.InputOffset() + MaxControlBytes + 1 }

func (s *statusStream) token() (json.Token, error) {
	s.window()
	return s.decoder.Token()
}

func (s *statusStream) delimiter(want json.Delim) error {
	token, err := s.token()
	if err != nil {
		return err
	}
	if token != want {
		return fmt.Errorf("%w: expected status delimiter %q", ErrUnknown, want)
	}
	return nil
}

func (s *statusStream) object(visit func(string) error) error {
	if err := s.delimiter('{'); err != nil {
		return err
	}
	seen := make(map[string]struct{})
	for {
		s.window()
		if !s.decoder.More() {
			break
		}
		token, err := s.token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("%w: invalid status object key", ErrUnknown)
		}
		folded := task.FoldJSONKey(key)
		if _, exists := seen[folded]; exists {
			return errors.Join(ErrUnknown, task.ErrDuplicateKey)
		}
		seen[folded] = struct{}{}
		if err := visit(key); err != nil {
			return err
		}
	}
	return s.delimiter('}')
}

func (s *statusStream) value() ([]byte, error) {
	start := s.decoder.InputOffset()
	// Decode consumes the colon and leading whitespace before the raw value.
	// Bound that syntax separately so formatting does not reduce the value's
	// allowance. At most one value plus one syntax window can be buffered.
	s.reader.limit = start + 2*MaxControlBytes + 2
	var raw json.RawMessage
	if err := s.decoder.Decode(&raw); err != nil {
		return nil, err
	}
	syntaxBytes := s.decoder.InputOffset() - start - int64(len(raw))
	if len(raw) > MaxControlBytes || syntaxBytes > MaxControlBytes+1 {
		return nil, ErrControlLimit
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, ErrUnknown
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if err := task.ValidateJSONStructure(raw); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func ParseQueueSnapshotStream(reader io.Reader, version string) (QueueSnapshot, error) {
	if version == "" {
		return QueueSnapshot{}, ErrBinding
	}
	s := newStatusStream(reader)
	snapshot := QueueSnapshot{Groups: make(map[string]Group)}
	var tasksFound, groupsFound bool
	err := s.object(func(key string) error {
		switch key {
		case "tasks":
			tasksFound = true
			return s.readTasks(&snapshot)
		case "groups":
			groupsFound = true
			return s.readGroups(&snapshot)
		default:
			_, err := s.value()
			return err
		}
	})
	if err != nil {
		return QueueSnapshot{}, errors.Join(ErrUnknown, err)
	}
	if !tasksFound || !groupsFound {
		return QueueSnapshot{}, fmt.Errorf("%w: missing status field", ErrUnknown)
	}
	s.window()
	var extra json.RawMessage
	if err = s.decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return QueueSnapshot{}, errors.Join(ErrUnknown, task.ErrTrailingJSON, err)
	}
	return snapshot, nil
}

func (s *statusStream) readTasks(snapshot *QueueSnapshot) error {
	return s.object(func(key string) error {
		id, err := ParseID(key)
		if err != nil {
			return err
		}
		raw, err := s.value()
		if err != nil {
			return err
		}
		job, err := parseJob(raw, id)
		if err != nil {
			return err
		}
		snapshot.Jobs = append(snapshot.Jobs, job)
		return nil
	})
}

func (s *statusStream) readGroups(snapshot *QueueSnapshot) error {
	return s.object(func(key string) error {
		raw, err := s.value()
		if err != nil {
			return err
		}
		// Reuse the existing group schema rather than implementing a second
		// interpretation for streamed responses.
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return err
		}
		encoded := append([]byte{'{'}, encodedKey...)
		encoded = append(encoded, ':')
		encoded = append(encoded, raw...)
		encoded = append(encoded, '}')
		groups, err := ParseGroups(encoded)
		if err != nil {
			return err
		}
		snapshot.Groups[key] = groups[key]
		return nil
	})
}
