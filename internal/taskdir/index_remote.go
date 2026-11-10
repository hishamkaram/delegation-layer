package taskdir

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RemoteIndexObservationNames retains at most the two newest cache generations
// while streaming existing names. It never opens a writable Reference or takes
// a control lock. Callers must validate the actual observation records and may
// encounter a concurrent cache-pruning absence.
func (r *ReadOnlyStore) RemoteIndexObservationNames(ctx context.Context, id string) (names []string, resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	directory, err := r.store.openDir(filepath.Join(r.store.Root, "remote-refs", id))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		entries, readErr := directory.Readdirnames(100)
		for _, name := range entries {
			names, err = retainObservationName(names, name)
			if err != nil {
				return nil, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			slices.Reverse(names)
			return names, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func retainObservationName(names []string, name string) ([]string, error) {
	suffix, ok := strings.CutPrefix(name, "observation-")
	if !ok {
		return names, nil
	}
	digits, ok := strings.CutSuffix(suffix, ".json")
	sequence, err := strconv.ParseUint(digits, 10, 64)
	if !ok || len(digits) != 20 || err != nil || sequence == 0 {
		return nil, task.ErrEvidenceFault
	}
	names = append(names, name)
	slices.Sort(names)
	return names[max(0, len(names)-2):], nil
}
