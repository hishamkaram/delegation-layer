package gatewayservice

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type directoryEntry struct {
	path string
	info os.FileInfo
}

// A failed barrier can leave either a visible publication or a visible removal.
// Every retry seals the whole existing ancestry, including previously created
// directories, before acknowledging either outcome. No creation journal is
// needed: the named service definition remains the installation authority.
func (m *Manager) barrierInstalledUnit() error {
	if err := m.barrierServiceAncestry(false); err != nil {
		return err
	}
	present, err := m.readUnit()
	if err != nil || !present {
		return errors.Join(ErrForeignService, err)
	}
	return nil
}

func (m *Manager) barrierUnitAbsence() error {
	if err := m.barrierServiceAncestry(true); err != nil {
		return err
	}
	present, err := m.readUnit()
	if err != nil {
		return err
	}
	if present {
		return ErrServiceChanged
	}
	return nil
}

func (m *Manager) barrierServiceAncestry(allowMissing bool) error {
	if err := m.checkServiceDirectory(); err != nil {
		return err
	}
	entries, err := directoryAncestry(m.config.Directory, allowMissing)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = m.barrierDirectory(entry); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		current, readErr := os.Lstat(entry.path)
		if readErr != nil || !os.SameFile(entry.info, current) {
			return errors.Join(ErrConfiguration, readErr)
		}
	}
	return nil
}

// The clean absolute path bounds traversal. Missing suffixes are allowed only
// for deletion confirmation: syncing the nearest existing parent seals their
// removal too. Ancestors need not be owned by the user (for example, /).
func directoryAncestry(path string, allowMissing bool) ([]directoryEntry, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrConfiguration
	}
	var entries []directoryEntry
	for {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist) && allowMissing && len(entries) == 0:
		case err != nil:
			return nil, err
		case !info.IsDir():
			return nil, ErrConfiguration
		default:
			entries = append(entries, directoryEntry{path: path, info: info})
		}
		parent := filepath.Dir(path)
		if path == parent {
			return entries, nil
		}
		path = parent
	}
}

func (m *Manager) barrierDirectory(entry directoryEntry) (resultErr error) {
	file, err := os.OpenFile(entry.path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	current, err := file.Stat()
	if err != nil || !os.SameFile(entry.info, current) {
		return errors.Join(ErrConfiguration, err)
	}
	return m.directoryBarrier(file)
}
