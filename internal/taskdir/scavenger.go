package taskdir

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

func IsRecognizedStageFile(name string) bool {
	if len(name) != 42 || !strings.HasPrefix(name, "stage.") || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	id := name[6:38]
	if strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (s *Store) Scavenge() (cleaned int, resultErr error) {
	if err := s.maintLock.LockEXNonblocking(); err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, s.maintLock.Unlock()) }()
	return s.scavengeDirectory(s.Root)
}

func (s *Store) scavengeDirectory(path string) (int, error) {
	dir, err := s.openDir(path)
	if err != nil {
		return 0, err
	}
	entries, err := dir.ReadDir(-1)
	err = errors.Join(err, dir.Close())
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, entry := range entries {
		name := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			count, e := s.scavengeDirectory(name)
			cleaned += count
			if e != nil {
				return cleaned, e
			}
			continue
		}
		if !IsRecognizedStageFile(entry.Name()) {
			continue
		}
		if _, e := s.exists(name); e != nil {
			return cleaned, e
		}
		if e := s.unlink(name); e != nil {
			return cleaned, e
		}
		if e := s.barrierDir(path); e != nil {
			return cleaned, e
		}
		cleaned++
	}
	return cleaned, nil
}
