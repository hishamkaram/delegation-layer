// Package remotedata owns recorded remote byte paths and rooted directory access.
// Records and lifecycle authority remain with their existing task/store owners.
package remotedata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type Role string

const (
	Origin      Role = "origin"
	Destination Role = "destination"
)

type Family string

const (
	Snapshot Family = "snapshot"
	Inputs   Family = "inputs"
	Outputs  Family = "outputs"
)

type Location struct {
	Root   string
	RootID string
	TaskID string
	Layout task.RemoteDataLayout
	Family Family
	Role   Role
}

func Path(layout task.RemoteDataLayout, root, rootID, id string, family Family) (string, error) {
	if err := layout.Validate(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", fmt.Errorf("remote data root must be canonical and absolute")
	}
	if err := task.ValidateRootID(rootID); err != nil {
		return "", err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return "", err
	}
	legacy := ""
	switch family {
	case Snapshot:
		legacy = "remote-snapshots"
	case Inputs:
		legacy = "remote-inputs"
	case Outputs:
		legacy = "remote-outputs"
	default:
		return "", fmt.Errorf("invalid remote data family")
	}
	if layout == task.RemoteDataLegacy {
		return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+"."+legacy, rootID, id), nil
	}
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".remote-data", rootID, id, string(family)), nil
}

func InputWorkspace(layout task.RemoteDataLayout, root, rootID, id string) (string, error) {
	p, e := Path(layout, root, rootID, id, Inputs)
	return filepath.Join(p, "workspace"), e
}

// Classify describes an owned family from a canonical native path; the caller
// validates its recorded root and task before granting authority.
func Classify(cwd string) (Location, bool, error) {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return Location{}, false, nil
	}
	volume := filepath.VolumeName(cwd)
	parts := strings.Split(strings.TrimPrefix(cwd[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		loc, ok := classifyComponent(parts, i, part)
		if !ok {
			continue
		}
		parent := volume + string(filepath.Separator) + filepath.Join(parts[:i]...)
		loc.Root = filepath.Join(parent, loc.Root)
		return loc, true, nil
	}
	return Location{}, false, nil
}

func classifyComponent(parts []string, index int, name string) (Location, bool) {
	var loc Location
	if !strings.HasPrefix(name, ".") || index+2 >= len(parts) {
		return loc, false
	}
	suffixes := []struct {
		suffix string
		family Family
		layout task.RemoteDataLayout
	}{
		{".remote-data", "", task.RemoteDataOrganized},
		{".remote-snapshots", Snapshot, task.RemoteDataLegacy},
		{".remote-inputs", Inputs, task.RemoteDataLegacy},
		{".remote-outputs", Outputs, task.RemoteDataLegacy},
	}
	for _, candidate := range suffixes {
		if strings.HasSuffix(name, candidate.suffix) {
			loc.Root = strings.TrimSuffix(name[1:], candidate.suffix)
			loc.Family, loc.Layout = candidate.family, candidate.layout
			break
		}
	}
	if loc.Root == "" {
		return loc, false
	}
	loc.RootID, loc.TaskID = parts[index+1], parts[index+2]
	if task.ValidateRootID(loc.RootID) != nil || task.ValidateTaskID(loc.TaskID) != nil {
		return loc, false
	}
	if loc.Layout == task.RemoteDataOrganized {
		if index+3 >= len(parts) {
			return loc, false
		}
		loc.Family = Family(parts[index+3])
	}
	switch loc.Family {
	case Snapshot:
		loc.Role = Origin
	case Inputs, Outputs:
		loc.Role = Destination
	default:
		return loc, false
	}
	return loc, true
}

func Open(layout task.RemoteDataLayout, root, rootID, id string, family Family, create bool) (*os.Root, error) {
	path, err := Path(layout, root, rootID, id, family)
	if err != nil {
		return nil, err
	}
	return openOwnedPath(filepath.Dir(root), path, create)
}

func openOwnedPath(parent, path string, create bool) (*os.Root, error) {
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return nil, err
	}
	current, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		next, e := OpenPrivate(current, part, create)
		e = errors.Join(e, current.Close())
		if e != nil {
			if next != nil {
				e = errors.Join(e, next.Close())
			}
			return nil, e
		}
		current = next
	}
	return current, nil
}

func OpenPrivate(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = privateOwner(info); err != nil {
		return nil, err
	}
	if create {
		f, e := parent.Open(".")
		if e != nil {
			return nil, e
		}
		if e = errors.Join(f.Sync(), f.Close()); e != nil {
			return nil, e
		}
	}
	opened, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	f, err := opened.Open(".")
	if err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	actual, statErr := f.Stat()
	err = errors.Join(statErr, f.Close())
	if err == nil {
		err = privateOwner(actual)
		if err == nil && !os.SameFile(info, actual) {
			err = errors.New("remote data directory was replaced")
		}
	}
	if err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	return opened, nil
}

// Remove deletes only a validated owned family and an empty organized task
// parent. Root-ID and top-level parents remain shared with other admissions.
func Remove(layout task.RemoteDataLayout, root, rootID, id string, family Family) error {
	path, err := Path(layout, root, rootID, id, family)
	if err != nil {
		return err
	}
	parent, err := openOwnedPath(filepath.Dir(root), filepath.Dir(path), false)
	if task.RecordAbsent(err) {
		return syncOwnedAbsence(filepath.Dir(root), filepath.Dir(path), syncRoot)
	}
	if err != nil {
		return err
	}
	err = errors.Join(removeFamily(parent, filepath.Base(path), syncRoot), parent.Close())
	if err != nil || layout != task.RemoteDataOrganized {
		return err
	}
	return pruneTaskParent(filepath.Dir(root), filepath.Dir(filepath.Dir(path)), id)
}

func removeFamily(parent *os.Root, name string, syncDirectory func(*os.Root) error) error {
	info, err := parent.Lstat(name)
	if task.RecordAbsent(err) {
		return syncDirectory(parent)
	}
	if err != nil {
		return err
	}
	if err = privateOwner(info); err != nil {
		return err
	}
	if err = parent.RemoveAll(name); err != nil {
		return err
	}
	return syncDirectory(parent)
}

func pruneTaskParent(base, parentPath, id string) error {
	parent, err := openOwnedPath(base, parentPath, false)
	if task.RecordAbsent(err) {
		return syncOwnedAbsence(base, parentPath, syncRoot)
	}
	if err != nil {
		return err
	}
	return errors.Join(removeEmpty(parent, id), syncRoot(parent), parent.Close())
}

// A prior removal may be visible after its barrier failed. A retry acknowledges
// absence only after syncing its nearest existing, validated parent.
func syncOwnedAbsence(base, path string, syncDirectory func(*os.Root) error) (resultErr error) {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.Join(err, errors.New("remote data parent escapes its root"))
	}
	current, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.Close()) }()
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		next, openErr := OpenPrivate(current, name, false)
		if task.RecordAbsent(openErr) {
			return syncDirectory(current)
		}
		if openErr != nil {
			return openErr
		}
		if closeErr := current.Close(); closeErr != nil {
			return errors.Join(closeErr, next.Close())
		}
		current = next
	}
	return syncDirectory(current)
}

func syncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
