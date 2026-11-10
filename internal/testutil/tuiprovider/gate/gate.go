// Package gate owns finite test-only rendezvous files. It creates no process
// or goroutine; Delegate's existing guardian still owns provider cancellation.
package gate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

const (
	Admission = "admission"
	Provider  = "provider"
	maxWait   = 45 * time.Second
)

// BeforeProvider preserves the contributor runtime's help, version and
// self-test paths. The existing adapter emits runtime-config first; all other
// argument arrangements remain the contributor parser's responsibility.
func BeforeProvider(args []string) error {
	if len(args) < 2 || args[0] != "--runtime-config" {
		return nil
	}
	file, err := os.Open(args[1])
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, task.MaxControlRecordSize+1))
	if err = errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	if len(data) > task.MaxControlRecordSize {
		return task.ErrControlRecordTooBig
	}
	var configuration protocol.RuntimeConfig
	if err := task.DecodeStrict(data, &configuration); err != nil {
		return err
	}
	return Wait(configuration.RuntimeDir, Provider, "provider-started")
}

// Wait acts only on an explicit hold marker. The test driver owns marker
// creation/removal; this function owns its entered receipt and finite wait.
func Wait(directory, point, identity string) error {
	return wait(directory, point, identity, maxWait)
}

func wait(directory, point, identity string, limit time.Duration) error {
	if directory == "" {
		return nil
	}
	if point != Admission && point != Provider {
		return errors.New("unknown TUI fixture rendezvous")
	}
	if err := validateRuntime(directory); err != nil {
		return err
	}
	marker := filepath.Join(directory, "hold-"+point)
	if _, markerErr := os.Lstat(marker); errors.Is(markerErr, os.ErrNotExist) {
		return nil
	} else if markerErr != nil {
		return markerErr
	}
	entered := filepath.Join(directory, point+"-entered")
	if err := recordEntered(entered, identity); err != nil {
		return err
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("TUI fixture %s barrier expired", point)
}

func validateRuntime(directory string) error {
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || !filepath.IsAbs(directory) || canonical != directory {
		return errors.Join(errors.New("TUI fixture runtime must be an existing canonical absolute directory"), err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("TUI fixture runtime must be a private directory")
	}
	return nil
}

func recordEntered(path, identity string) (resultErr error) {
	stage := path + ".pending"
	file, err := os.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.Remove(stage)) }()
	_, writeErr := fmt.Fprintln(file, identity)
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	// Readers observe either no receipt or the complete identity. A second
	// entrance cannot overwrite the first fixture owner's receipt.
	return os.Link(stage, path)
}
