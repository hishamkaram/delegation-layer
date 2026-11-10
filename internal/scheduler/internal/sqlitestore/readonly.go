package sqlitestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

// OpenReadOnly observes the existing DELETE-journal database without obtaining
// a RESERVED writer lock or permitting recovery writes. The scheduler remains
// the sole writer; this connection cannot initialize or migrate it.
func OpenReadOnly(ctx context.Context, root, rootID string) (*Store, error) {
	path, err := existingDatabase(root, rootID)
	if err != nil {
		return nil, err
	}
	if err = checkReadOnlyHeader(path); err != nil {
		return nil, err
	}
	query := url.Values{"mode": {"ro"}, "_pragma": connectionPragmas(runtime.GOOS)}
	location := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: query.Encode()}
	connector, err := sqlite.NewConnector(location.String())
	if err != nil {
		return nil, err
	}
	return openWithConnector(ctx, connector, path, rootID, runtime.GOOS)
}

// A read-only WAL connection may create shared-memory sidecars before SQL
// guards run. Refuse its documented header format before opening SQLite. This
// is not an atomic defence against external same-UID path/mode replacement.
func checkReadOnlyHeader(path string) (resultErr error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var opened unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &opened); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&opened, os.Geteuid(), false) != taskdir.PrivateStatAccepted {
		return errUnsafeFile
	}
	var header [20]byte
	n, err := io.ReadFull(file, header[:])
	if n == 0 && errors.Is(err, io.EOF) {
		return errNotInitialized
	}
	if err != nil {
		return errors.Join(errForeignDatabase, err)
	}
	if !bytes.Equal(header[:16], []byte("SQLite format 3\x00")) {
		return errForeignDatabase
	}
	if header[18] != 1 || header[19] != 1 {
		return errJournalMode
	}
	return nil
}
