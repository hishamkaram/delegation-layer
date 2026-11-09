package remotedata

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type Measurement struct {
	Bytes     int64
	Entries   int
	Truncated bool
	Changed   bool
}

type usageWalk struct {
	ctx    context.Context
	root   *os.Root
	limit  int
	result Measurement
}

// Measure walks bounded native directory batches without following symlinks.
// Entries which disappear during the walk still count against the bound.
func Measure(ctx context.Context, root *os.Root, limit int) (Measurement, error) {
	if limit <= 0 {
		return Measurement{}, errors.New("remote usage entry bound must be positive")
	}
	walk := usageWalk{ctx: ctx, root: root, limit: limit}
	err := walk.directory(".")
	return walk.result, err
}

func (w *usageWalk) directory(path string) (resultErr error) {
	directory, err := openDirectory(w.root, path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	for {
		if err = w.ctx.Err(); err != nil {
			return err
		}
		// One look-ahead entry distinguishes an exact bound from an unfinished walk.
		batch, e := directory.ReadDir(min(100, w.limit-w.result.Entries+1))
		for _, entry := range batch {
			if w.result.Entries == w.limit {
				w.result.Truncated = true
				return nil
			}
			w.result.Entries++
			if err = w.entry(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
			if w.result.Truncated {
				return nil
			}
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
		if e != nil {
			return e
		}
	}
}

func (w *usageWalk) entry(path string) error {
	info, err := w.root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		w.result.Changed = true
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		w.result.Bytes += info.Size()
		return nil
	}
	if !info.IsDir() {
		return nil
	}
	err = w.directory(path)
	if errors.Is(err, os.ErrNotExist) {
		w.result.Changed = true
		return nil
	}
	return err
}
