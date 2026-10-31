package sqlitestore

import (
	"net/url"
	"path/filepath"
	"strconv"
)

const (
	// DirectoryName is the private scheduler directory under a state root.
	DirectoryName = ".scheduler"
	databaseName  = "schedules.sqlite"

	busyTimeoutMillis = 5000
	// synchronousExtra is PRAGMA synchronous=EXTRA as read back.
	synchronousExtra = 3
)

// Directory returns the scheduler directory of a state root.
func Directory(root string) string { return filepath.Join(root, DirectoryName) }

// DatabasePath returns the scheduler database of a state root.
func DatabasePath(root string) string { return filepath.Join(Directory(root), databaseName) }

// connectionPragmas are connection-local settings every physical connection
// applies when it opens. None changes the database file: journal_mode and
// auto_vacuum are persistent, so they are never set. The guard verifies
// journal_mode; auto_vacuum is left at the SQLite default.
func connectionPragmas(goos string) []string {
	pragmas := []string{
		"busy_timeout(" + strconv.Itoa(busyTimeoutMillis) + ")",
		"foreign_keys(1)",
		"synchronous(" + strconv.Itoa(synchronousExtra) + ")",
	}
	if goos == "darwin" {
		pragmas = append(pragmas, "fullfsync(1)")
	}
	return pragmas
}

// databaseURI builds the one canonical SQLite file URI. mode=rw takes effect
// only in a file: URI and never creates a missing database; net/url escapes
// every special path character, so no part of the path becomes a parameter.
// _txlock=immediate makes every transaction BEGIN IMMEDIATE.
func databaseURI(path, goos string) string {
	query := url.Values{
		"mode":    {"rw"},
		"_pragma": connectionPragmas(goos),
		"_txlock": {"immediate"},
	}
	location := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: query.Encode()}
	return location.String()
}
