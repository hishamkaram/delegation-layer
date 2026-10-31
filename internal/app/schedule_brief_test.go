package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type scheduleBriefErrorCase struct {
	name   string
	want   error
	change func(string) error
}

func scheduleBriefErrorCases() []scheduleBriefErrorCase {
	return []scheduleBriefErrorCase{
		{name: "missing", want: config.ErrBriefNotFound, change: os.Remove},
		{name: "not regular", want: config.ErrBriefNotRegular, change: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0o700)
		}},
		{name: "too large", want: config.ErrBriefTooLarge, change: func(path string) error {
			return os.Truncate(path, task.MaxBriefSize+1)
		}},
		{name: "empty", want: config.ErrBriefEmpty, change: func(path string) error {
			return os.Truncate(path, 0)
		}},
	}
}

func TestScheduleBriefReadClassifiesChangedFile(t *testing.T) {
	for _, tc := range scheduleBriefErrorCases() {
		t.Run(tc.name, func(t *testing.T) {
			a := scheduleCreateArgs(t)
			var observed error
			_, err := readScheduleBrief(a.Brief, func(path string) ([]byte, error) {
				if changeErr := tc.change(path); changeErr != nil {
					return nil, changeErr
				}
				data, readErr := tasklifecycle.ReadBrief(path)
				observed = readErr
				return data, fmt.Errorf("read brief: %w", readErr)
			})
			if !errors.Is(observed, tc.want) {
				t.Fatalf("real ReadBrief err=%v; want %v", observed, tc.want)
			}
			if !errors.Is(err, tc.want) || !errors.Is(err, errScheduleInput) {
				t.Fatalf("changed brief classification err=%v; want typed invalid-input %v", err, tc.want)
			}
		})
	}
}

func TestScheduleCreateBriefErrorsAreInvalidInput(t *testing.T) {
	for _, tc := range scheduleBriefErrorCases() {
		t.Run(tc.name, func(t *testing.T) {
			a := scheduleCreateArgs(t)
			if err := tc.change(a.Brief); err != nil {
				t.Fatal(err)
			}
			response, code := runScheduleCreateResponse(t, a)
			if code != 2 || response.Error == "" {
				t.Fatalf("invalid brief code=%d response=%+v", code, response)
			}
			if _, err := os.Lstat(a.Root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid brief initialized root: %v", err)
			}
		})
	}
}

func TestScheduleBriefReadRetainsJoinedStorageFault(t *testing.T) {
	a := scheduleCreateArgs(t)
	storageFault := errors.New("brief close fault")
	_, err := readScheduleBrief(a.Brief, func(path string) ([]byte, error) {
		if removeErr := os.Remove(path); removeErr != nil {
			return nil, removeErr
		}
		data, readErr := tasklifecycle.ReadBrief(path)
		return data, errors.Join(fmt.Errorf("read brief: %w", readErr), storageFault)
	})
	if !errors.Is(err, config.ErrBriefNotFound) || !errors.Is(err, storageFault) || errors.Is(err, errScheduleInput) {
		t.Fatalf("joined brief fault changed classification or lost a cause: %v", err)
	}
}

func TestScheduleBriefReadClassifiesBoundedReadLimit(t *testing.T) {
	// ReadBrief uses the same typed bounded-read error if a file grows after
	// opening. Obtain it from that API without racing a filesystem write.
	_, sizeErr := task.ReadBounded(strings.NewReader("xx"), 1)
	if !errors.Is(sizeErr, task.ErrControlRecordTooBig) {
		t.Fatalf("bounded-read limit err=%v", sizeErr)
	}
	a := scheduleCreateArgs(t)
	_, err := readScheduleBrief(a.Brief, func(string) ([]byte, error) {
		return nil, fmt.Errorf("read brief: %w", sizeErr)
	})
	if !errors.Is(err, sizeErr) || !errors.Is(err, errScheduleInput) {
		t.Fatalf("bounded-read brief error lost its typed input classification: %v", err)
	}
}
