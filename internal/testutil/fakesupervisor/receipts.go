package fakesupervisor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func newInvocationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate invocation ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func writeEntryReceipt(dir string, receipt EntryReceipt) error {
	if err := ensureReceiptDir(dir); err != nil {
		return err
	}
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, receipt.InvocationID+".entry.json"), data, 0o600, MaxReceiptBytes)
}

func writeCompletionReceipt(dir string, receipt CompletionReceipt) error {
	if err := ensureReceiptDir(dir); err != nil {
		return err
	}
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, receipt.InvocationID+".completion.json"), data, 0o600, MaxReceiptBytes)
}

func hashOutput(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func outputReceipt(data []byte) (int64, string) {
	return int64(len(data)), hashOutput(data)
}

func receiptTime() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func ensureReceiptDir(path string) error {
	return ensurePrivateDir(path)
}

func outputError(err error) []byte {
	return []byte(err.Error() + "\n")
}

func canonicalOutputPath(path string) error {
	if path == "" {
		return nil
	}
	if _, err := cleanAbsolute(path); err != nil {
		return err
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil && canonical != path {
		return fmt.Errorf("%w: output path must not be a symlink", ErrInvalidConfig)
	}
	return nil
}

func readConfiguredOutput(path string) ([]byte, error) {
	if err := canonicalOutputPath(path); err != nil {
		return nil, err
	}
	return readOutput(path)
}
