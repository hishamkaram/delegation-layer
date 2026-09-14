package taskdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// PrepareLaunchFiles materializes only admitted, non-secret configuration and
// assigns fresh native output paths. It requires the consumed runner permit;
// no adapter receives a storage handle or chooses a filesystem destination.
func (td *TaskDir) PrepareLaunchFiles(arguments []string) (_ []string, resultErr error) {
	state, err := td.ownedRunner()
	if err != nil {
		return nil, err
	}
	defer state.mu.Unlock()
	if state.sealed || state.poisoned != nil || state.filesPrepared {
		return nil, task.ErrEvidenceFault
	}
	state.filesPrepared = true
	defer func() { state.poisoned = errors.Join(state.poisoned, resultErr) }()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	args := append([]string(nil), arguments...)
	if err = td.materializeInputs(meta.InputFiles, args); err != nil {
		return nil, err
	}
	if len(meta.OutputArtifacts) > 0 {
		if err = td.createLaunchDirectory("provider-output"); err != nil {
			return nil, err
		}
	}
	for _, output := range meta.OutputArtifacts {
		if err = bindLaunchPath(args, output.ArgumentIndex, filepath.Join(td.Dir, "provider-output", output.Name)); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func bindLaunchPath(args []string, index int, path string) error {
	if index < 0 || index >= len(args) || args[index] != "" {
		return fmt.Errorf("%w: invalid reserved launch argument", task.ErrIdentityMismatch)
	}
	args[index] = path
	return nil
}

func (td *TaskDir) createLaunchDirectory(name string) error {
	dir, err := td.store.openDir(td.Dir)
	if err != nil {
		return err
	}
	err = unix.Mkdirat(int(dir.Fd()), name, 0o700)
	err = errors.Join(err, dir.Close())
	if err != nil {
		return err // EEXIST is a fault, never reuse native staging.
	}
	return td.store.barrierDirectoryEntry(filepath.Join(td.Dir, name), td.Dir, td.store.faultInjector)
}

func (td *TaskDir) materializeInputs(inputs []task.InputFile, args []string) error {
	if len(inputs) == 0 {
		return nil
	}
	if err := td.createLaunchDirectory("provider-input"); err != nil {
		return err
	}
	dir := filepath.Join(td.Dir, "provider-input")
	for _, input := range inputs {
		if err := bindLaunchPath(args, input.ArgumentIndex, filepath.Join(dir, input.Name)); err != nil {
			return err
		}
		_, cleanup, err := td.store.stageAndCommit(dir, input.Name, []byte(input.Content), td.store.faultInjector)
		if err = errors.Join(err, cleanup); err != nil {
			return err
		}
	}
	return nil
}

// admittedRawNames includes only declared optional outputs that were imported
// into raw. Unrelated directory contents can never become evidence.
func (td *TaskDir) admittedRawNames() ([]string, error) {
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	names := []string{"stderr", "stdout"}
	for _, output := range meta.OutputArtifacts {
		exists, err := td.store.exists(filepath.Join(td.Dir, "raw", output.Name))
		if err != nil {
			return nil, err
		}
		if exists {
			names = append(names, output.Name)
		}
	}
	return names, nil
}

func (td *TaskDir) validateManifestDeclarations(manifest []task.RawManifestEntry) error {
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	allowed := map[string]bool{"raw/stdout": true, "raw/stderr": true}
	for _, output := range meta.OutputArtifacts {
		allowed["raw/"+output.Name] = true
	}
	for _, entry := range manifest {
		if !allowed[entry.Path] {
			return fmt.Errorf("%w: undeclared raw artifact", task.ErrEvidenceFault)
		}
	}
	return nil
}

// openNativeOutput tolerates native-created read permissions inside a private
// 0700 directory, but never follows links or accepts writable-by-others files.
// Imported protocol evidence always uses the core's stricter 0600 mode.
func (td *TaskDir) openNativeOutput(name string) (*os.File, error) {
	dir, err := td.store.openDir(filepath.Join(td.Dir, "provider-output"))
	if err != nil {
		return nil, fmt.Errorf("%w: native output directory unavailable: %w", task.ErrEvidenceFault, err)
	}
	defer closeFileQuietly(dir)
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNativeOutputAbsent
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(td.Dir, "provider-output", name))
	var st unix.Stat_t
	info, statErr := f.Stat()
	err = errors.Join(statErr, unix.Fstat(fd, &st))
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1) {
		err = task.ErrEvidenceFault
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
