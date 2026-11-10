package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

func cachePath(environment []string, root string) (string, error) {
	if environment == nil {
		return "", errors.New("history cache resolution requires a captured environment")
	}
	value := func(key string) string { return verifiedexec.EnvironmentValue(environment, key) }
	directory := value("XDG_CACHE_HOME")
	if runtime.GOOS == "darwin" {
		directory = filepath.Join(value("HOME"), "Library", "Caches")
	} else if directory == "" && value("HOME") != "" {
		directory = filepath.Join(value("HOME"), ".cache")
	}
	if directory == "" || !filepath.IsAbs(directory) || (runtime.GOOS == "darwin" && value("HOME") == "") {
		return "", errors.New("captured user cache directory is missing or relative")
	}
	canonical, err := config.CanonicalizePath(directory)
	if err != nil {
		return "", err
	}
	// Hash only the canonical root pathname, never executable content.
	path := filepath.Join(canonical, "delegation-layer", "history", "v1", task.ComputeSHA256([]byte(root))+".sqlite")
	if relative, err := filepath.Rel(root, path); err != nil || filepath.IsLocal(relative) {
		return "", errors.New("history cache must remain outside the execution root")
	}
	return path, nil
}

func (c *Catalog) openCache(ctx context.Context, forceRebuild bool) error {
	if err := prepareCacheDirectories(filepath.Dir(c.path)); err != nil {
		return err
	}
	var err error
	c.useLease, err = taskdir.OpenLockFileContext(ctx, c.path+".use", taskdir.LockLevelNone)
	if err != nil {
		return err
	}
	c.writer, err = taskdir.OpenLockFileContext(ctx, c.path+".scanner", taskdir.LockLevelNone)
	if err != nil {
		return err
	}
	if err = c.useLease.LockSHNonblockingContext(ctx); err != nil {
		return errors.Join(ErrCacheBusy, err)
	}
	if err = privateCacheFiles(c.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if forceRebuild {
		c.rebuilt = err == nil
		return c.rebuild(ctx)
	}
	if err == nil {
		if err = c.connect(ctx); err == nil {
			err = c.verifyCache(ctx)
		}
		if err == nil {
			return nil
		}
		if !rebuildable(err) {
			return err
		}
		c.rebuilt = true
	}
	return c.rebuild(ctx)
}

func (c *Catalog) connect(ctx context.Context) error {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(c.path)}
	uri.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(1000)", "synchronous(NORMAL)", "temp_store(FILE)"}}.Encode()
	connector := c.connector
	if connector == nil {
		var err error
		connector, err = sqlite.NewConnector(uri.String())
		if err != nil {
			return err
		}
	}
	c.db = sql.OpenDB(connector)
	c.db.SetMaxOpenConns(3)
	c.db.SetMaxIdleConns(3)
	return c.db.PingContext(ctx)
}

func (c *Catalog) verifyCache(ctx context.Context) error {
	var version int
	if err := c.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return ErrCacheInvalid
	}
	var recorded string
	if err := c.db.QueryRowContext(ctx, "SELECT binding FROM catalog WHERE singleton=1").Scan(&recorded); err != nil {
		return errors.Join(ErrCacheInvalid, err)
	}
	expected, err := c.binding()
	if err != nil {
		return err
	}
	if recorded != expected {
		return ErrCacheInvalid
	}
	return privateCacheFiles(c.path)
}

func (c *Catalog) rebuild(ctx context.Context) (resultErr error) {
	if err := c.closeDatabase(); err != nil {
		return err
	}
	if err := c.useLease.Unlock(); err != nil {
		return err
	}
	// Never upgrade a shared lease or unlink lock files. Other database users
	// make a rebuild visibly unavailable, not permission to replace live WAL.
	if err := c.useLease.LockEXNonblockingContext(ctx); err != nil {
		return errors.Join(ErrCacheBusy, err)
	}
	exclusive := true
	defer func() {
		if c.rebuildLeaseReleasable(exclusive) {
			resultErr = errors.Join(resultErr, c.useLease.Unlock())
		}
	}()
	if err := removeCacheFiles(c.path); err != nil {
		return err
	}
	file, err := os.OpenFile(c.path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = c.connect(ctx); err != nil {
		return err
	}
	if err = c.initialize(ctx); err != nil {
		return err
	}
	// Downgrade by releasing first. No database handle survives the gap.
	if err = c.closeDatabase(); err != nil {
		return err
	}
	if err = c.useLease.Unlock(); err != nil {
		return err
	}
	exclusive = false
	if err = c.useLease.LockSHNonblockingContext(ctx); err != nil {
		return errors.Join(ErrCacheBusy, err)
	}
	if err := c.connect(ctx); err != nil {
		return err
	}
	return c.verifyCache(ctx)
}

// rebuildLeaseReleasable leaves EX under Catalog custody until its database has
// closed successfully. Failed startup cleanup must not expose live WAL files.
func (c *Catalog) rebuildLeaseReleasable(exclusive bool) bool {
	return exclusive && c.db == nil && c.dbCloseErr == nil
}

// removeCacheFiles requires the caller's exclusive cache-use lease. Stable
// lock identities are intentionally not part of the disposable SQLite set.
func removeCacheFiles(database string) error {
	for _, suffix := range []string{"-shm", "-wal", "-journal", ""} {
		path := database + suffix
		if err := privatePath(path, false); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) initialize(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	value, err := c.binding()
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, "INSERT INTO catalog(singleton,binding,incomplete,last_scan) VALUES(1,?,1,0)", value)
	return err
}

func rebuildable(err error) bool {
	if errors.Is(err, ErrCacheInvalid) {
		return true
	}
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code()&0xff == 11 || sqliteErr.Code()&0xff == 26)
}

func lockBusy(err error) bool {
	return errors.Is(err, task.ErrLockBusy) || errors.Is(err, unix.EWOULDBLOCK)
}

func privateCacheFiles(path string) error {
	if err := privatePath(path, false); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := privatePath(path+suffix, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func privatePath(path string, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&stat, os.Geteuid(), directory) != taskdir.PrivateStatAccepted {
		return fmt.Errorf("history cache path must be private and owned: %s", path)
	}
	return nil
}

func prepareCacheDirectories(directory string) error {
	// Only the application-owned three-level subtree must be private. An
	// existing XDG cache parent is user-owned configuration, not ours to chmod.
	base := filepath.Dir(filepath.Dir(directory))
	for _, path := range []string{base, filepath.Join(base, "history"), directory} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		if err := privatePath(path, true); err != nil {
			return err
		}
	}
	return nil
}

const schema = `
CREATE TABLE catalog(singleton INTEGER PRIMARY KEY CHECK(singleton=1),binding TEXT NOT NULL,incomplete INTEGER NOT NULL,last_scan INTEGER NOT NULL,scan_faults INTEGER NOT NULL DEFAULT 0);
CREATE TABLE entries(kind TEXT NOT NULL,id TEXT NOT NULL,provider TEXT NOT NULL DEFAULT '',model TEXT NOT NULL DEFAULT '',effort TEXT NOT NULL DEFAULT '',cwd TEXT NOT NULL DEFAULT '',workspace TEXT NOT NULL DEFAULT '',team TEXT NOT NULL DEFAULT '',member TEXT NOT NULL DEFAULT '',device TEXT NOT NULL DEFAULT '',created TEXT NOT NULL DEFAULT '',request_digest TEXT NOT NULL DEFAULT '',metadata_digest TEXT NOT NULL DEFAULT '',search TEXT NOT NULL DEFAULT '',fault TEXT NOT NULL DEFAULT '',observed TEXT NOT NULL DEFAULT '',admission INTEGER NOT NULL DEFAULT 0,liveness INTEGER NOT NULL DEFAULT 0,publication INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL DEFAULT '',failure TEXT NOT NULL DEFAULT '',source TEXT NOT NULL DEFAULT '',observation_fault TEXT NOT NULL DEFAULT '',refreshed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(kind,id));
CREATE INDEX entries_created ON entries(created DESC,kind,id);
CREATE INDEX entries_observed ON entries(observed DESC,kind,id);
PRAGMA user_version=1;`
