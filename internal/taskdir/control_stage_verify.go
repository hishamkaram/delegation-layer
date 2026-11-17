package taskdir

import (
	"bytes"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// VerifyStageRecord recognizes a duplicate staging artifact only when its exact
// bytes and the named committed record both match the caller's validated value.
// It performs bounded rooted reads only: no deletion, barrier or write probe.
// Matching bytes do not prove a previously failed durability barrier succeeded.
func (tx *ControlTransaction) VerifyStageRecord(stageName, recordName string, record any) error {
	if err := tx.enter(); err != nil {
		return err
	}
	defer tx.leave()
	if !IsRecognizedStageFile(stageName) {
		return task.ErrEvidenceFault
	}
	if err := validateControlName(recordName); err != nil {
		return err
	}
	data, err := marshalControlRecord(record)
	if err != nil {
		return err
	}
	for _, name := range []string{recordName, stageName} {
		saved, readErr := tx.d.store.readBytes(filepath.Join(tx.d.dir, name), int64(len(data)))
		if readErr != nil {
			return fmt.Errorf("control evidence %s: %w", name, readErr)
		}
		if !bytes.Equal(saved, data) {
			return fmt.Errorf("%w: duplicate stage disagrees with %s", task.ErrEvidenceFault, recordName)
		}
	}
	return nil
}
