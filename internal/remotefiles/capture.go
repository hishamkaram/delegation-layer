package remotefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/filepaths"
)

// Provenance describes the input and task whose post-closure workspace was
// observed. Capture does not claim exclusive authorship of an external cwd.
type Provenance struct {
	OriginRootID string `json:"origin_root_id"`
	RootID       string `json:"root_id"`
	TaskID       string `json:"task_id"`
	InputSHA256  string `json:"input_sha256"`
}

type CapturedFile struct {
	Reason     string `json:"reason,omitempty"`
	CapturedAt string `json:"captured_at,omitempty"`
	Path       string `json:"path"`
	Status     string `json:"status"`
	Entry      *Entry `json:"entry,omitempty"`
}

type Receipt struct {
	CompletedAt string         `json:"completed_at,omitempty"`
	Provenance  Provenance     `json:"provenance"`
	CapturedAt  string         `json:"captured_at"`
	Files       []CapturedFile `json:"files"`
}

// Capture freezes declared output files into a private destination. Its caller
// must prove canonical closure before calling it, limit ctx to the finite
// auxiliary budget, serialize with the capture journal, and persist the receipt
// there only after success. This routine never changes the task outcome.
func Capture(ctx context.Context, source, destination *os.Root, selectors []string, provenance Provenance) (Receipt, error) {
	return CaptureWithLimit(ctx, source, destination, selectors, provenance, MaxTotalBytes)
}

// CaptureWithLimit applies the caller's remaining aggregate budget during both
// source description and copying; recovery must pass its journal-derived remainder.
func CaptureWithLimit(ctx context.Context, source, destination *os.Root, selectors []string, provenance Provenance, byteLimit int64) (Receipt, error) {
	if byteLimit < 0 || byteLimit > MaxTotalBytes {
		return Receipt{}, fmt.Errorf("invalid capture byte limit")
	}
	receipt := Receipt{Provenance: provenance, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Files: make([]CapturedFile, 0, len(selectors))}
	if err := filepaths.ValidateSelectors(selectors); err != nil {
		return Receipt{}, err
	}
	var total int64
	for _, name := range selectors {
		file := captureSelectedFile(ctx, source, destination, name, byteLimit-total)
		if file.Entry != nil {
			total += file.Entry.Size
		}
		receipt.Files = append(receipt.Files, file)
	}
	receipt.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)

	return receipt, nil
}

// ValidateSelectors checks the complete declared output set before admission.
func ValidateSelectors(selectors []string) error { return filepaths.ValidateSelectors(selectors) }

func copyCaptured(ctx context.Context, source, destination *os.Root, entry Entry) (resultErr error) {
	input, err := openRegular(source, entry.Path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, captureIO(input.Close())) }()
	return publishCopy(ctx, destination, entry, input)
}

// publishCopy writes a private partial and publishes a verified no-replace link.
// The caller's journal transaction serializes recovery of the owned partial.
func publishCopy(ctx context.Context, destination *os.Root, entry Entry, input io.Reader) error {
	if err := mkdirParents(destination, entry.Path); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(entry.Path))
	partial := ".delegate-" + hex.EncodeToString(sum[:]) + ".part"
	if err := settleCopy(destination, partial, entry.Path); err != nil {
		return err
	}
	if _, err := destination.Lstat(entry.Path); err == nil {
		return verifyDestination(ctx, destination, entry)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writePartial(ctx, destination, partial, entry, input); err != nil {
		return err
	}
	if err := destination.Link(partial, entry.Path); err != nil {
		return err
	}
	if err := barrierRoot(destination); err != nil {
		return err
	}
	if err := destination.Remove(partial); err != nil {
		return err
	}
	return barrierRoot(destination)
}

func writePartial(ctx context.Context, destination *os.Root, partial string, entry Entry, input io.Reader) error {
	mode := os.FileMode(0o644)
	if entry.Executable {
		mode = 0o755
	}
	output, err := destination.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	digest := sha256.New()
	count, copyErr := copyContext(ctx, io.MultiWriter(output, digest), io.LimitReader(input, entry.Size))
	if copyErr == nil {
		var extra [1]byte
		n, readErr := io.ReadFull(input, extra[:])
		if n > 0 {
			copyErr = invalidSource("changed", fmt.Errorf("%w: source grew during capture %q", ErrConflict, entry.Path))
		} else if readErr != nil && !errors.Is(readErr, io.EOF) {
			copyErr = readErr
		}
	}
	if copyErr == nil && (count != entry.Size || hex.EncodeToString(digest.Sum(nil)) != entry.SHA256) {
		copyErr = invalidSource("changed", fmt.Errorf("%w: source changed during capture %q", ErrConflict, entry.Path))
	}
	if copyErr == nil {
		copyErr = output.Chmod(mode)
	}
	if copyErr == nil {
		copyErr = barrierFile(output)
	}
	return errors.Join(copyErr, captureIO(output.Close()))
}

func settleCopy(root *os.Root, partial, final string) error {
	old, err := root.Lstat(partial)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !old.Mode().IsRegular() {
		return fmt.Errorf("capture partial must be regular")
	}
	published, err := root.Lstat(final)
	if err == nil && !os.SameFile(old, published) {
		return fmt.Errorf("%w: capture partial disagrees with published file", ErrConflict)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.Remove(partial); err != nil {
		return err
	}
	return barrierRoot(root)
}

func verifyDestination(ctx context.Context, root *os.Root, entry Entry) error {
	actual, err := describeFile(ctx, root, entry.Path)
	if err != nil {
		return &captureIOError{err}
	}
	if actual.Size != entry.Size || actual.SHA256 != entry.SHA256 || actual.Executable != entry.Executable {
		return fmt.Errorf("%w: destination %q", ErrConflict, entry.Path)
	}
	return nil
}

// ValidateCapturedFile checks the relationship between an availability report
// and its immutable descriptor before a reader may select captured bytes.
func ValidateCapturedFile(file CapturedFile) error {
	if len(file.Reason) > 256 {
		return fmt.Errorf("capture reason exceeds bound")
	}
	if file.CapturedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, file.CapturedAt); err != nil {
			return err
		}
	}
	if err := ValidatePath(file.Path); err != nil {
		return err
	}
	switch file.Status {
	case "absent", "rejected", "over_budget", "incomplete":
		if file.Entry != nil {
			return fmt.Errorf("unavailable file has a descriptor")
		}
		if file.Status != "absent" && file.Reason == "" {
			return fmt.Errorf("unavailable file has no reason")
		}
	case "captured":
		if file.Entry == nil || file.Entry.Path != file.Path {
			return fmt.Errorf("captured file descriptor does not match path")
		}
		return ValidateManifest(Manifest{Entries: []Entry{*file.Entry}})
	default:
		return fmt.Errorf("invalid capture availability %q", file.Status)
	}
	return nil
}

func captureSelectedFile(ctx context.Context, source, destination *os.Root, name string, remaining int64) CapturedFile {
	file := CapturedFile{Path: name, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if ctx.Err() != nil {
		file.Status = "incomplete"
		file.Reason = "capture_deadline"
		return file
	}
	entry, err := describeFileWithLimit(ctx, source, name, remaining)
	if errors.Is(err, os.ErrNotExist) {
		file.Status = "absent"
		return file
	}
	if err != nil {
		return captureFailure(ctx, file, err)
	}
	if err = copyCaptured(ctx, source, destination, entry); err != nil {
		return captureFailure(ctx, file, err)
	}
	file.Status = "captured"
	file.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	file.Entry = &entry
	return file
}

// Source validity is distinct from an interrupted filesystem operation. The
// receipt exposes stable reason codes, never unrestricted filesystem errors.
type sourceValidityError struct {
	reason string
	err    error
}

func (e *sourceValidityError) Error() string { return e.err.Error() }
func (e *sourceValidityError) Unwrap() error { return e.err }
func invalidSource(reason string, err error) error {
	return &sourceValidityError{reason: reason, err: err}
}

// Destination validation can reuse source readers, but its failures describe
// capture I/O and must not reject the user's selected source.
func captureIO(err error) error {
	if err == nil {
		return nil
	}
	return &captureIOError{err}
}

type captureIOError struct{ err error }

func (e *captureIOError) Error() string { return e.err.Error() }
func (e *captureIOError) Unwrap() error { return e.err }

func captureFailure(ctx context.Context, file CapturedFile, err error) CapturedFile {
	file.Status, file.Reason = "incomplete", "io_error"
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		file.Reason = "capture_deadline"
		return file
	}
	var ioFailure *captureIOError
	if errors.As(err, &ioFailure) {
		return file
	}
	var invalid *sourceValidityError
	if errors.As(err, &invalid) {
		file.Status, file.Reason = "rejected", invalid.reason
		if invalid.reason == "aggregate_byte_limit" {
			file.Status = "over_budget"
		}
	}
	return file
}
