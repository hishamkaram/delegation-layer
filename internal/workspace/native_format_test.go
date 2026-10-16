package workspace

import (
	"path/filepath"
	"strings"
	"testing"
)

// Git's stable porcelain contract permits both a bare lock attribute and a
// reason. With -z the reason can contain literal newlines.
// https://git-scm.com/docs/git-worktree#_porcelain_format
func TestWorkspaceRegistrationAcceptsNativeLockReasons(t *testing.T) {
	path, pathErr := filepath.EvalSymlinks(t.TempDir())
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	for _, lock := range []string{"locked", "locked added with --lock", "locked reason\nwith newline"} {
		t.Run(lock, func(t *testing.T) {
			data := []byte("worktree " + path + "\x00HEAD " + strings.Repeat("a", 40) + "\x00detached\x00" + lock + "\x00\x00")
			registrations, err := parseWorktreeRegistrations(data)
			if err != nil {
				t.Fatal(err)
			}
			registration, found, matchErr := matchWorktreeRegistration(registrations, path)
			if matchErr != nil || !found || !registration.locked {
				t.Fatalf("native Git lock attribute was ignored: %q", lock)
			}
		})
	}
}
