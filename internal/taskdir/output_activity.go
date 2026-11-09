package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// OutputActivity is approximate captured-write metadata, never process or
// publication authority. Empty files and Delegate startup diagnostics are excluded.
type OutputActivity struct {
	LastProviderOutputAt string `json:"last_provider_output_at,omitempty"`
	Source               string `json:"source"`
}

func (td *TaskDir) ReadOutputActivityContext(ctx context.Context) (*OutputActivity, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if _, err := td.readStartedEvidence(); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var last time.Time
	for _, name := range []string{"stdout", "stderr"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := td.store.openFile(filepath.Join(td.Dir, "raw", name), os.O_RDONLY)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if err = errors.Join(statErr, closeErr); err != nil {
			return nil, err
		}
		if info.Size() > 0 && info.ModTime().After(last) {
			last = info.ModTime()
		}
	}
	if last.IsZero() {
		return nil, nil
	}
	return &OutputActivity{LastProviderOutputAt: last.UTC().Format(time.RFC3339Nano), Source: "capture_metadata"}, nil
}
