package inspectionfixture

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type helperMarker struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         string `json:"phase"`
}

// RunHelper executes the finite native helper. Its only argument is the
// absolute helper-config path from inspection-fixture.json. It creates a
// unique invocation directory, emits the private sentinel on both streams,
// waits for the configured finite delay, and writes a completion marker only
// after its bounded success output has been accepted by the caller's pipes.
func RunHelper(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) != 1 || stdout == nil || stderr == nil {
		return ErrInvalidHelper
	}
	loaded, err := loadHelperConfig(args[0])
	if err != nil {
		return ErrInvalidHelper
	}
	sentinel, err := readCanonicalRegular(loaded.Config.SentinelPath, maxSentinelBytes, false)
	if err != nil || len(sentinel) == 0 {
		clear(sentinel)
		return ErrInvalidHelper
	}
	if !utf8.Valid(sentinel) {
		clear(sentinel)
		return ErrInvalidHelper
	}
	stderrSentinel := append([]byte(nil), sentinel...)
	invocation, err := newInvocationDirectory(loaded.Config.ArtifactDir)
	if err != nil {
		clear(sentinel)
		return ErrInvalidHelper
	}
	if err = writeMarker(filepath.Join(invocation, "started.json"), "started"); err != nil {
		clear(sentinel)
		return ErrInvalidHelper
	}
	output, err := task.MarshalCanonical(helperOutput{
		SchemaVersion: SchemaVersion,
		Sentinel:      string(sentinel),
		Eligible:      loaded.Config.Eligible,
	})
	clear(sentinel)
	if err != nil {
		clear(stderrSentinel)
		return ErrHelperOutput
	}
	if err = writeAll(stderr, stderrSentinel); err != nil {
		clear(stderrSentinel)
		clear(output)
		return ErrHelperOutput
	}
	clear(stderrSentinel)
	if err = writeAll(stdout, output); err != nil {
		clear(output)
		return ErrHelperOutput
	}
	clear(output)
	if loaded.Config.DelayMS > 0 {
		time.Sleep(time.Duration(loaded.Config.DelayMS) * time.Millisecond)
	}
	if err = writeMarker(filepath.Join(invocation, "completed.json"), "completed"); err != nil {
		return ErrInvalidHelper
	}
	return nil
}

func newInvocationDirectory(artifactDir string) (string, error) {
	invocations := filepath.Join(artifactDir, "invocations")
	if err := os.MkdirAll(invocations, 0o700); err != nil {
		return "", err
	}
	for attempt := 0; attempt < 3; attempt++ {
		id, err := task.NewRandomID()
		if err != nil {
			return "", err
		}
		path := filepath.Join(invocations, id)
		if err = os.Mkdir(path, 0o700); err == nil {
			return path, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("inspection fixture invocation collision")
}

func writeMarker(path, phase string) error {
	data, err := task.MarshalCanonical(helperMarker{SchemaVersion: SchemaVersion, Phase: phase})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writeErr := writeAll(file, data)
	syncErr := file.Sync()
	closeErr := file.Close()
	clear(data)
	return errors.Join(writeErr, syncErr, closeErr)
}
