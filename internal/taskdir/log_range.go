package taskdir

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const MaxLogRangeBytes int64 = 64 << 10

var (
	ErrLogObjectReplaced = errors.New("log object replaced")
	ErrLogTruncated      = errors.New("log truncated")
	ErrLogCursor         = errors.New("invalid log cursor")
)

// LogRange is a bounded observation. WholeSHA256 is expected sealed metadata;
// RangeSHA256 describes only Data. A live range grants no publication authority.
type LogRange struct {
	Stream       string `json:"stream"`
	Offset       int64  `json:"offset"`
	Length       int64  `json:"length"`
	ObservedSize int64  `json:"observed_size"`
	Sealed       bool   `json:"sealed"`
	WholeSHA256  string `json:"whole_sha256,omitempty"`
	RangeSHA256  string `json:"range_sha256,omitempty"`
	Cursor       string `json:"cursor"`
	Data         []byte `json:"-"`
}

type logCursor struct {
	Version int    `json:"version"`
	RootID  string `json:"root_id"`
	TaskID  string `json:"task_id"`
	Stream  string `json:"stream"`
	Object  string `json:"object"`
	Offset  int64  `json:"offset"`
	Size    int64  `json:"size"`
}

func parseLogCursor(value string) (logCursor, error) {
	var cursor logCursor
	if len(value) > 2048 {
		return cursor, ErrLogCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, ErrLogCursor
	}
	if err = task.DecodeStrict(data, &cursor); err != nil {
		return cursor, ErrLogCursor
	}
	if cursor.Version != 1 || cursor.Offset < 0 || cursor.Size < cursor.Offset || (cursor.Stream != "stdout" && cursor.Stream != "stderr") {
		return cursor, ErrLogCursor
	}
	if task.ValidateRootID(cursor.RootID) != nil || task.ValidateTaskID(cursor.TaskID) != nil || task.ValidateSHA256(cursor.Object) != nil {
		return cursor, ErrLogCursor
	}
	return cursor, nil
}

// LogCursorOffset decodes an observation offset only. ReadTaskLogRange checks
// its task, stream and native object before using it for a read.
func LogCursorOffset(value string) (int64, error) {
	cursor, err := parseLogCursor(value)
	return cursor.Offset, err
}

func ValidateLogCursor(value, rootID, id, stream string, offset int64) error {
	cursor, err := parseLogCursor(value)
	if err != nil {
		return err
	}
	if cursor.RootID != rootID || cursor.TaskID != id || cursor.Stream != stream || cursor.Offset != offset {
		return ErrLogCursor
	}
	return nil
}

func (r *ReadOnlyStore) ReadTaskLogRange(ctx context.Context, id, stream string, offset, limit int64, expected string) (result LogRange, resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return result, err
	}
	if err := validateLogRangeInput(id, stream, offset, limit); err != nil {
		return result, err
	}
	cursor, err := boundLogCursor(expected, r.store.RootID, id, stream, offset)
	if err != nil {
		return result, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	descriptor, err := td.logRangeDescriptor(ctx, stream)
	if err != nil {
		return result, err
	}
	file, err := r.store.openFile(filepath.Join(td.Dir, "raw", stream), os.O_RDONLY)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, object, err := logFileIdentity(file, r.store.RootID, id, stream)
	if err != nil {
		return result, err
	}
	if err = cursor.checkObject(object, info.Size(), offset); err != nil {
		return result, err
	}
	if descriptor.Sealed && (descriptor.Size == nil || *descriptor.Size != info.Size()) {
		return result, task.ErrEvidenceFault
	}
	data, err := readLogRangeBytes(file, offset, min(limit, info.Size()-offset))
	if err != nil {
		return result, err
	}
	result, err = observedLogRange(logCursor{Version: 1, RootID: r.store.RootID, TaskID: id, Stream: stream, Object: object, Offset: offset, Size: info.Size()}, descriptor, data)
	return result, errors.Join(err, ctx.Err())
}

func validateLogRangeInput(id, stream string, offset, limit int64) error {
	if err := task.ValidateTaskID(id); err != nil {
		return err
	}
	if (stream != "stdout" && stream != "stderr") || offset < 0 || limit < 1 || limit > MaxLogRangeBytes {
		return ErrLogCursor
	}
	return nil
}

func readLogRangeBytes(file *os.File, offset, length int64) ([]byte, error) {
	data := make([]byte, int(length))
	n, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(n) != length {
		return nil, ErrLogTruncated
	}
	return data, nil
}

func observedLogRange(cursor logCursor, descriptor LogDescriptor, data []byte) (LogRange, error) {
	result := LogRange{Stream: cursor.Stream, Offset: cursor.Offset, Length: int64(len(data)), ObservedSize: cursor.Size, Sealed: descriptor.Sealed, WholeSHA256: descriptor.SHA256, Data: data}
	if len(data) > 0 {
		result.RangeSHA256 = task.ComputeSHA256(data)
	}
	cursor.Offset += int64(len(data))
	cursorData, err := json.Marshal(cursor)
	if err != nil {
		return result, err
	}
	result.Cursor = base64.RawURLEncoding.EncodeToString(cursorData)
	return result, nil
}

func (c logCursor) checkObject(object string, size, offset int64) error {
	if c.Object != "" && c.Object != object {
		return ErrLogObjectReplaced
	}
	if (c.Object != "" && size < c.Size) || offset > size {
		return ErrLogTruncated
	}
	return nil
}

func boundLogCursor(expected, rootID, id, stream string, offset int64) (logCursor, error) {
	if expected == "" {
		return logCursor{}, nil
	}
	if err := ValidateLogCursor(expected, rootID, id, stream, offset); err != nil {
		return logCursor{}, err
	}
	return parseLogCursor(expected)
}

func (td *TaskDir) logRangeDescriptor(ctx context.Context, stream string) (LogDescriptor, error) {
	descriptors, err := td.logObservationDescriptors(ctx)
	if err != nil {
		return LogDescriptor{}, err
	}
	for _, descriptor := range descriptors {
		if descriptor.Path == filepath.Join(td.Dir, "raw", stream) && descriptor.Available {
			return descriptor, nil
		}
	}
	return LogDescriptor{}, os.ErrNotExist
}

func logFileIdentity(file *os.File, rootID, id, stream string) (os.FileInfo, string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if stat.Nlink != 1 || !info.Mode().IsRegular() {
		return nil, "", task.ErrEvidenceFault
	}
	object := task.ComputeSHA256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%d\x00%d", rootID, id, stream, stat.Dev, stat.Ino))
	return info, object, nil
}
