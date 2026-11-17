package resultread

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// Saved describes verified caller-owned output, not a new task outcome.
type Saved struct {
	Path   string `json:"path"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

// One pinned directory owns the private stage and atomic publication names.
type resultDestination struct {
	directory  *os.File
	stage      *os.File
	stageName  string
	targetName string
}

func openResultDestination(ctx context.Context, root, output string) (*resultDestination, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(parent, directorySearchFlags, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), parent)
	destination := &resultDestination{directory: directory, targetName: filepath.Base(output)}
	if err = destination.requireOutsideState(ctx, root); err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	name := ".delegate-result-" + rand.Text() + ".partial"
	stageFD, err := unix.Openat(fd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	destination.stageName = name
	destination.stage = os.NewFile(uintptr(stageFD), filepath.Join(parent, name))
	return destination, nil
}

func (d *resultDestination) cleanup() error {
	var err error
	if d.stage != nil {
		err = d.stage.Close()
		d.stage = nil
	}
	if d.stageName != "" {
		if removeErr := unix.Unlinkat(int(d.directory.Fd()), d.stageName, 0); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove owned result stage %s: %w", filepath.Join(d.directory.Name(), d.stageName), removeErr))
		}
	}
	return errors.Join(err, d.directory.Close())
}

// Save owns source and closes it exactly once on every return. It verifies the
// complete payload into a private stage, then publishes without replacing any
// existing destination. A non-nil Saved remains authoritative if cleanup fails.
func Save(ctx context.Context, root, output string, descriptor task.PayloadDescriptor, source io.ReadCloser) (saved *Saved, err error) {
	if source == nil {
		return nil, errors.New("result source is required")
	}
	defer func() {
		if source != nil {
			err = errors.Join(err, source.Close())
		}
	}()
	if ctx == nil {
		return nil, context.Canceled
	}
	destination, err := openResultDestination(ctx, root, output)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, destination.cleanup()) }()
	copyErr := copyVerified(ctx, source, descriptor, destination.stage)
	sourceErr := source.Close()
	source = nil
	closeErr := destination.stage.Close()
	destination.stage = nil
	if err = errors.Join(copyErr, sourceErr, closeErr); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return destination.publish(ctx, root, output, descriptor)
}

func (d *resultDestination) publish(ctx context.Context, root, output string, descriptor task.PayloadDescriptor) (*Saved, error) {
	if err := d.requireOutsideState(ctx, root); err != nil {
		return nil, err
	}
	visible, err := os.Stat(filepath.Dir(output))
	if err != nil {
		return nil, err
	}
	pinned, err := d.directory.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(visible, pinned) {
		return nil, task.ErrRequestConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd := int(d.directory.Fd())
	if err := unix.Linkat(fd, d.stageName, fd, d.targetName, 0); err != nil {
		return nil, err
	}
	return &Saved{Path: output, Length: descriptor.Length, SHA256: descriptor.SHA256}, nil
}

// Native identity, rather than lexical spelling, also rejects case aliases on macOS.
func (d *resultDestination) requireOutsideState(ctx context.Context, root string) (err error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return err
	}
	fd, err := unix.Openat(int(d.directory.Fd()), ".", directorySearchFlags, 0)
	if err != nil {
		return err
	}
	current := os.NewFile(uintptr(fd), d.directory.Name())
	defer func() { err = errors.Join(err, current.Close()) }()
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		info, statErr := current.Stat()
		if statErr != nil {
			return statErr
		}
		if os.SameFile(rootInfo, info) {
			return errors.New("result output must be outside the selected state root")
		}
		parentFD, openErr := unix.Openat(int(current.Fd()), "..", directorySearchFlags, 0)
		if openErr != nil {
			return openErr
		}
		parent := os.NewFile(uintptr(parentFD), "result destination ancestor")
		parentInfo, statErr := parent.Stat()
		if statErr != nil {
			return errors.Join(statErr, parent.Close())
		}
		closeErr := current.Close()
		current = parent
		if closeErr != nil {
			return closeErr
		}
		if os.SameFile(info, parentInfo) {
			return nil
		}
	}
}
