//go:build linux || darwin

package lifetime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Parent and directory handles pin invocation-created identity. A pathname
// alone, a failed Wait or an endpoint's absence never authorizes removal.
type nativeSchedulerDirectory struct {
	path       string
	home       string
	name       string
	parent     *os.Root
	root       *os.Root
	parentInfo os.FileInfo
	info       os.FileInfo
}

func newNativeSchedulerDirectory() (*nativeSchedulerDirectory, error) {
	owner := &nativeSchedulerDirectory{}
	home, err := os.UserHomeDir()
	if err != nil {
		return owner, err
	}
	owner.home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return owner, err
	}
	owner.parent, err = os.OpenRoot(owner.home)
	if err != nil {
		return owner, err
	}
	owner.parentInfo, err = owner.parent.Stat(".")
	if err != nil {
		return owner, err
	}
	owner.path, err = os.MkdirTemp(owner.home, ".delegate-native-scheduler-signals-")
	if err != nil {
		return owner, err
	}
	owner.name = filepath.Base(owner.path)
	owner.root, err = owner.parent.OpenRoot(owner.name)
	if err != nil {
		return owner, err
	}
	owner.info, err = owner.root.Stat(".")
	if err != nil {
		return owner, err
	}
	if err = owner.check(); err != nil {
		return owner, err
	}
	return owner, nativeSchedulerOutsideWorkspace(owner.path)
}

func nativeSchedulerOutsideWorkspace(path string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// Go tests run in their package directory. Check the complete enclosing
	// module, rather than just this package's working directory.
	for directory := cwd; ; directory = filepath.Dir(directory) {
		if _, statErr := os.Stat(filepath.Join(directory, "go.mod")); statErr == nil {
			relative, relErr := filepath.Rel(directory, path)
			if relErr != nil || filepath.IsLocal(relative) {
				return fmt.Errorf("native acceptance base must be outside the workspace")
			}
			return nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if filepath.Dir(directory) == directory {
			return fmt.Errorf("cannot verify native acceptance workspace boundary")
		}
	}
}

func (d *nativeSchedulerDirectory) check() error {
	if d.parent == nil || d.root == nil || d.info == nil || d.parentInfo == nil {
		return fmt.Errorf("native directory identity is not pinned")
	}
	parent, err := os.Lstat(d.home)
	if err != nil {
		return err
	}
	if !parent.IsDir() || !os.SameFile(parent, d.parentInfo) {
		return fmt.Errorf("native fixture parent was replaced")
	}
	named, err := d.parent.Lstat(d.name)
	if err != nil {
		return err
	}
	if !named.IsDir() || !os.SameFile(named, d.info) {
		return fmt.Errorf("native fixture directory was replaced")
	}
	return nil
}

func (d *nativeSchedulerDirectory) write(name string, data []byte) error {
	if err := d.check(); err != nil {
		return err
	}
	file, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	return errors.Join(err, file.Close())
}

func (d *nativeSchedulerDirectory) finish(remove bool) (resultErr error) {
	if remove && d.path != "" {
		if err := d.check(); err != nil {
			resultErr = err
		} else {
			resultErr = d.parent.RemoveAll(d.name)
		}
	}
	if d.root != nil {
		resultErr = errors.Join(resultErr, d.root.Close())
	}
	if d.parent != nil {
		resultErr = errors.Join(resultErr, d.parent.Close())
	}
	return resultErr
}
