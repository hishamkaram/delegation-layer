package app

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// This is permission to remove one exact historical DATA seed created without
// actors. It grants no lifetime closure and never applies to public-created IDs.
type publicContinuationSeedData struct {
	root, id, payload string
	snapshot          publicContinuationSeedSnapshot
}

func capturePublicContinuationSeed(td *taskdir.TaskDir, payload string) (publicContinuationSeedData, error) {
	root := filepath.Dir(filepath.Dir(td.Dir))
	snapshot, err := readPublicContinuationSeedSnapshot(root, td.TaskID, payload, false)
	return publicContinuationSeedData{root: root, id: td.TaskID, payload: payload, snapshot: snapshot}, err
}

func (p publicContinuationSeedData) check(td *taskdir.TaskDir) error {
	if td == nil || p.root == "" || p.id != td.TaskID || td.Dir != filepath.Join(p.root, "tasks", p.id) {
		return task.ErrIdentityMismatch
	}
	current, err := readPublicContinuationSeedSnapshot(p.root, p.id, p.payload, false)
	if err != nil {
		return err
	}
	if !samePublicContinuationSeedSnapshot(p.snapshot, current) {
		return task.ErrIdentityMismatch
	}
	return nil
}

type publicContinuationSeedFile struct {
	info os.FileInfo
	sha  string
}

type publicContinuationSeedSnapshot struct {
	root, tasks, task os.FileInfo
	rootRecord        publicContinuationSeedFile
	files             map[string]publicContinuationSeedFile
}

func samePublicContinuationSeedFile(a, b publicContinuationSeedFile) bool {
	return os.SameFile(a.info, b.info) && a.info.Mode() == b.info.Mode() &&
		a.info.Size() == b.info.Size() && a.info.ModTime().Equal(b.info.ModTime()) && a.sha == b.sha
}

func samePublicContinuationSeedSnapshot(a, b publicContinuationSeedSnapshot) bool {
	if !os.SameFile(a.root, b.root) || !os.SameFile(a.tasks, b.tasks) || !os.SameFile(a.task, b.task) ||
		!samePublicContinuationSeedFile(a.rootRecord, b.rootRecord) || len(a.files) != len(b.files) {
		return false
	}
	for name, original := range a.files {
		current, found := b.files[name]
		if !found || !samePublicContinuationSeedFile(original, current) {
			return false
		}
	}
	return true
}

func readPublicContinuationSeedSnapshot(root, id, payload string, outputDirectory bool) (snapshot publicContinuationSeedSnapshot, resultErr error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || task.ValidateTaskID(id) != nil {
		return snapshot, task.ErrIdentityMismatch
	}
	rootFD, err := openPublicContinuationSeedFile(nil, root, true)
	if err != nil {
		return snapshot, err
	}
	defer func() { resultErr = errors.Join(resultErr, rootFD.Close()) }()
	snapshot.rootRecord, err = readPublicContinuationSeedRootRecord(rootFD)
	if err != nil {
		return snapshot, err
	}
	tasks, err := openPublicContinuationSeedFile(rootFD, "tasks", true)
	if err != nil {
		return snapshot, err
	}
	defer func() { resultErr = errors.Join(resultErr, tasks.Close()) }()
	taskFD, err := openPublicContinuationSeedFile(tasks, id, true)
	if err != nil {
		return snapshot, err
	}
	defer func() { resultErr = errors.Join(resultErr, taskFD.Close()) }()
	snapshot.files, err = readPublicContinuationSeedTaskData(taskFD, payload, outputDirectory)
	if err != nil {
		return snapshot, err
	}
	if err = checkPublicContinuationSeedFile(rootFD, tasks, "tasks", true); err != nil {
		return snapshot, err
	}
	if err = checkPublicContinuationSeedFile(tasks, taskFD, id, true); err != nil {
		return snapshot, err
	}
	snapshot.root, err = rootFD.Stat()
	if err != nil {
		return snapshot, err
	}
	named, err := os.Lstat(root)
	if err != nil || !os.SameFile(snapshot.root, named) {
		return snapshot, errors.Join(task.ErrIdentityMismatch, err)
	}
	snapshot.tasks, err = tasks.Stat()
	if err == nil {
		snapshot.task, err = taskFD.Stat()
	}
	return snapshot, err
}

func readPublicContinuationSeedRootRecord(rootFD *os.File) (publicContinuationSeedFile, error) {
	record, err := openPublicContinuationSeedFile(rootFD, "root.json", false)
	if err != nil {
		return publicContinuationSeedFile{}, err
	}
	stamp, readErr := readPublicContinuationSeedFile(rootFD, record, "root.json")
	return stamp, errors.Join(readErr, record.Close())
}

func readPublicContinuationSeedTaskData(taskFD *os.File, payload string, outputDirectory bool) (map[string]publicContinuationSeedFile, error) {
	allowed := map[string]bool{
		".admission.lock": false, ".run.lock": false, "brief.md": false, "task.json": false, "meta.json": false,
		"provider.start": false, "provider.started.json": false, "provider.exit": false,
		"provider.ref.json": false, "outcome.json": false, "raw": true,
	}
	if _, collision := allowed[payload]; collision || payload == "" || filepath.Base(payload) != payload {
		return nil, task.ErrEvidenceFault
	}
	allowed[payload] = false // Basename comes from the validated registered-predicate outcome.
	if outputDirectory {
		allowed["provider-output"] = true // PrepareLaunchFiles creates it; the no-actor writer never fills it.
	}
	files := make(map[string]publicContinuationSeedFile)
	return files, readPublicContinuationSeedDirectory(taskFD, "", allowed, files)
}

func readPublicContinuationSeedDirectory(parent *os.File, prefix string, allowed map[string]bool, files map[string]publicContinuationSeedFile) error {
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != len(allowed) {
		return fmt.Errorf("%w: unexpected historical seed inventory: %v", task.ErrEvidenceFault, entries)
	}
	for _, entry := range entries {
		directory, accepted := allowed[entry.Name()]
		if !accepted {
			return fmt.Errorf("%w: unexpected historical seed entry %s%s", task.ErrEvidenceFault, prefix, entry.Name())
		}
		file, openErr := openPublicContinuationSeedFile(parent, entry.Name(), directory)
		if openErr != nil {
			return openErr
		}
		stamp, readErr := readPublicContinuationSeedFile(parent, file, entry.Name())
		if readErr == nil && directory {
			children := map[string]bool{}
			if entry.Name() == "raw" {
				children = map[string]bool{"stdout": false, "stderr": false}
			}
			readErr = readPublicContinuationSeedDirectory(file, entry.Name()+"/", children, files)
		}
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		files[prefix+entry.Name()] = stamp
	}
	return nil
}

func openPublicContinuationSeedFile(parent *os.File, name string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	var fd int
	var err error
	if parent == nil {
		fd, err = unix.Open(name, flags, 0)
	} else if name != "" && name != "." && name != ".." && filepath.Base(name) == name {
		fd, err = unix.Openat(int(parent.Fd()), name, flags, 0)
	} else {
		return nil, task.ErrEvidenceFault
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, errors.Join(task.ErrEvidenceFault, unix.Close(fd))
	}
	if err := checkPublicContinuationSeedFile(parent, file, name, directory); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func checkPublicContinuationSeedFile(parent, file *os.File, name string, directory bool) error {
	var held, named unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &held); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&held, os.Geteuid(), directory) != taskdir.PrivateStatAccepted || (!directory && held.Nlink != 1) {
		return task.ErrEvidenceFault
	}
	if parent == nil {
		return nil
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if held.Dev != named.Dev || held.Ino != named.Ino || held.Mode != named.Mode {
		return task.ErrIdentityMismatch
	}
	return nil
}

func readPublicContinuationSeedFile(parent, file *os.File, name string) (publicContinuationSeedFile, error) {
	before, err := file.Stat()
	if err != nil {
		return publicContinuationSeedFile{}, err
	}
	stamp := publicContinuationSeedFile{info: before}
	if !before.IsDir() {
		digest := sha256.New()
		if _, err = io.Copy(digest, file); err != nil {
			return stamp, err
		}
		stamp.sha = fmt.Sprintf("%x", digest.Sum(nil))
	}
	if err = checkPublicContinuationSeedFile(parent, file, name, before.IsDir()); err != nil {
		return stamp, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return stamp, errors.Join(task.ErrIdentityMismatch, err)
	}
	return stamp, nil
}
