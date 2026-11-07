package taskdir

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// RemoveCacheRecord removes one exact disposable cache generation under the
// enclosing transaction. Family-number names belong to the cache owner; fixed
// authority records and message-ID records cannot be addressed by this API.
// The owner must retain verified newer generations before retiring an older one.
func (tx *ControlTransaction) RemoveCacheRecord(family string, sequence uint64, expected any) error {
	if err := tx.enter(); err != nil {
		return err
	}
	defer tx.leave()
	name, err := cacheRecordName(family, sequence)
	if err != nil {
		return err
	}
	data, err := marshalControlRecord(expected)
	if err != nil {
		return err
	}
	if err = tx.d.verifyCanonicalRecord(name, data); err != nil {
		return err
	}
	if err = tx.ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(tx.d.dir, name)
	if injector := tx.d.store.faultInjector; injector != nil {
		if err = injector.OnCleanupUnlink(path); err != nil {
			return err
		}
	}
	directory, err := tx.d.store.openDir(tx.d.dir)
	if err != nil {
		return err
	}
	err = errors.Join(unix.Unlinkat(int(directory.Fd()), name, 0), directory.Close())
	if err != nil {
		return err
	}
	if injector := tx.d.store.faultInjector; injector != nil {
		if err = injector.OnCleanupDirBarrier(tx.d.dir); err != nil {
			return err
		}
	}
	return tx.d.store.barrierDir(tx.d.dir)
}

func cacheRecordName(family string, sequence uint64) (string, error) {
	if sequence == 0 || len(family) == 0 || len(family) > 24 {
		return "", task.ErrEvidenceFault
	}
	for index, c := range family {
		if c < 'a' || c > 'z' {
			if index == 0 || c < '0' || c > '9' {
				return "", task.ErrEvidenceFault
			}
		}
	}
	return fmt.Sprintf("%s-%020d.json", family, sequence), nil
}
