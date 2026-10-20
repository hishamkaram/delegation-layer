package workspaces

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/workspace"

	"golang.org/x/sys/unix"
)

// ValidateOwnedPlacement admits a new owned directory only when it is not,
// does not contain, and is not inside any protected directory. The new path
// may not exist yet: its existing prefix must already be canonical, and every
// existing ancestor is compared by directory identity so a symlink or mount
// alias cannot hide an overlap. Sibling directories of one shared container
// remain independent.
func ValidateOwnedPlacement(path string, protected []string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return workspace.ErrIdentity
	}
	existing, err := existingAncestor(path)
	if err != nil {
		return err
	}
	if err = VerifyCanonicalDirectory(existing); err != nil {
		return errors.Join(workspace.ErrIdentity, err)
	}
	ancestors, err := ancestorIdentities(existing)
	if err != nil {
		return err
	}
	for _, guarded := range protected {
		if err = rejectPlacementOverlap(path, existing, ancestors, guarded); err != nil {
			return err
		}
	}
	return nil
}

func rejectPlacementOverlap(path, existing string, ancestors map[workspace.DirectoryIdentity]struct{}, guarded string) error {
	canonical, err := filepath.EvalSymlinks(guarded)
	if err != nil {
		return fmt.Errorf("resolve protected workspace path: %w", err)
	}
	for _, candidate := range []string{guarded, canonical} {
		if workspace.PathWithin(candidate, path) || workspace.PathWithin(path, candidate) {
			return fmt.Errorf("%w: owned directory overlaps a protected path", workspace.ErrIdentity)
		}
	}
	identity, err := statIdentity(canonical)
	if err != nil {
		return err
	}
	if _, inside := ancestors[identity]; inside {
		return fmt.Errorf("%w: owned directory aliases a protected path", workspace.ErrIdentity)
	}
	if existing != path {
		return nil
	}
	// A retained directory may already exist; a protected path below it,
	// including through an alias, is still an overlap.
	owned, err := statIdentity(existing)
	if err != nil {
		return err
	}
	contained, err := ancestorIdentities(canonical)
	if err != nil {
		return err
	}
	if _, inside := contained[owned]; inside {
		return fmt.Errorf("%w: protected path is inside the owned directory", workspace.ErrIdentity)
	}
	return nil
}

func existingAncestor(path string) (string, error) {
	for current := path; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(current) == current {
			return "", workspace.ErrIdentity
		}
	}
}

func ancestorIdentities(path string) (map[workspace.DirectoryIdentity]struct{}, error) {
	identities := make(map[workspace.DirectoryIdentity]struct{})
	for current := path; ; current = filepath.Dir(current) {
		identity, err := statIdentity(current)
		if err != nil {
			return nil, err
		}
		identities[identity] = struct{}{}
		if filepath.Dir(current) == current {
			return identities, nil
		}
	}
}

func statIdentity(path string) (workspace.DirectoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return workspace.DirectoryIdentity{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return workspace.DirectoryIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}
