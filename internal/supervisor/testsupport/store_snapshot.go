package testsupport

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type StoreSnapshotFile struct {
	mode     fs.FileMode
	modified time.Time
	contents string
}

func SnapshotStore(t *testing.T, root string) map[string]StoreSnapshotFile {
	t.Helper()
	files := make(map[string]StoreSnapshotFile)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		file := StoreSnapshotFile{mode: info.Mode(), modified: info.ModTime()}
		// Opening a store runs its normal temporary filesystem probe. Compare
		// its entries and mode, while allowing that container's mtime to change.
		if path == filepath.Join(root, ".probe") {
			file.modified = time.Time{}
		}
		if !entry.IsDir() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			file.contents = string(data)
		}
		files[path] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
