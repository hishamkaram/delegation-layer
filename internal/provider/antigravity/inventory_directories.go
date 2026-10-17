package antigravity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func (b *inventoryBuilder) inspectDirectory(path string, scope SourceScope, kind SourceKind) (snapshot DirectorySnapshot, returnErr error) {
	if err := validateDirectoryPath(path); err != nil {
		return DirectorySnapshot{}, err
	}
	if _, exists := b.directoryKeys[path]; exists {
		return DirectorySnapshot{}, inventoryError("directory was enumerated twice: %s", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		missing := DirectorySnapshot{Path: path, Scope: scope, Kind: kind}
		b.directoryKeys[path] = struct{}{}
		b.inventory.Directories = append(b.inventory.Directories, missing)
		return missing, nil
	}
	if err != nil {
		return DirectorySnapshot{}, inventoryError("cannot inspect directory %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return DirectorySnapshot{}, inventoryError("configured path is not a real directory: %s", path)
	}
	entries, err := readStableDirectoryEntries(path, info)
	if err != nil {
		return DirectorySnapshot{}, err
	}
	snapshot = DirectorySnapshot{Path: path, Scope: scope, Kind: kind, Present: true, Entries: entries}
	if b.entries > MaxInventoryEntries-len(snapshot.Entries) {
		return DirectorySnapshot{}, inventoryTooLarge("total directory entry count")
	}
	b.entries += len(snapshot.Entries)
	snapshot.SHA256 = directoryDigest(snapshot.Entries)
	b.directoryKeys[path] = struct{}{}
	b.inventory.Directories = append(b.inventory.Directories, snapshot)
	return snapshot, nil
}

func validateDirectoryPath(path string) error {
	if filepath.Clean(path) != path || !filepath.IsAbs(path) {
		return inventoryError("directory is not an absolute canonical path: %s", path)
	}
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path {
		return inventoryError("directory is not canonical: %s", path)
	}
	return nil
}

func readStableDirectoryEntries(path string, before os.FileInfo) (result []DirectoryEntry, returnErr error) {
	file, err := openStablePolicyDirectory(path, before)
	if err != nil {
		return nil, inventoryError("cannot open configured directory %s: %v", path, err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	result, err = readDirectoryEntries(file, path)
	if err != nil {
		if errors.Is(err, ErrInventoryTooLarge) {
			return nil, err
		}
		return nil, inventoryError("cannot enumerate configured directory %s: %v", path, err)
	}
	if err := checkPolicyDirectoryAfterRead(path, file, before, result); err != nil {
		if errors.Is(err, ErrInventoryTooLarge) {
			return nil, err
		}
		return nil, inventoryError("configured directory changed during read %s: %v", path, err)
	}
	return result, returnErr
}

func openStablePolicyDirectory(path string, before os.FileInfo) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, errors.Join(errors.New("invalid policy directory descriptor"), unix.Close(fd))
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, errors.Join(errors.New("policy directory changed before read"), file.Close())
	}
	return file, nil
}

func readDirectoryEntries(file *os.File, path string) (result []DirectoryEntry, returnErr error) {
	entries, err := file.ReadDir(MaxInventoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > MaxInventoryEntries {
		return nil, inventoryTooLarge("directory entry count")
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	result = make([]DirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		entryKind, classifyErr := classifyDirectoryEntry(entry)
		if classifyErr != nil {
			return nil, fmt.Errorf("unsafe configured entry %s: %w", filepath.Join(path, entry.Name()), classifyErr)
		}
		result = append(result, DirectoryEntry{Name: entry.Name(), Kind: entryKind})
	}
	return result, nil
}

func checkPolicyDirectoryAfterRead(path string, file *os.File, opened os.FileInfo, expected []DirectoryEntry) error {
	final, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !final.IsDir() || !named.IsDir() || !os.SameFile(final, opened) || !os.SameFile(named, opened) ||
		final.Size() != opened.Size() || !final.ModTime().Equal(opened.ModTime()) {
		return errors.New("policy directory changed during read")
	}
	// Directory mtime resolution is filesystem-dependent and can remain
	// unchanged after an entry is created. Re-open the same canonical inode and
	// compare membership so the stability check does not rely on wall-clock
	// metadata precision.
	currentFile, err := openStablePolicyDirectory(path, opened)
	if err != nil {
		return err
	}
	current, readErr := readDirectoryEntries(currentFile, path)
	closeErr := currentFile.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !slices.Equal(current, expected) {
		return errors.New("policy directory membership changed during read")
	}
	return nil
}

func classifyDirectoryEntry(entry os.DirEntry) (string, error) {
	mode := entry.Type()
	if mode&os.ModeSymlink != 0 {
		return "", errors.New("symlink is not an admissible source")
	}
	if mode.IsDir() {
		return "directory", nil
	}
	if mode.IsRegular() {
		return "file", nil
	}
	info, err := entry.Info()
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("symlink is not an admissible source")
	}
	if info.Mode().IsDir() {
		return "directory", nil
	}
	if info.Mode().IsRegular() {
		return "file", nil
	}
	return "", errors.New("nonregular source")
}

func directoryDigest(entries []DirectoryEntry) string {
	var encoded strings.Builder
	for _, entry := range entries {
		encoded.WriteString(entry.Name)
		encoded.WriteByte(0)
		encoded.WriteString(entry.Kind)
		encoded.WriteByte(0)
	}
	return task.ComputeSHA256([]byte(encoded.String()))
}

func membershipDigest(directories []DirectorySnapshot) string {
	var encoded strings.Builder
	for _, directory := range directories {
		encoded.WriteString(directory.Path)
		encoded.WriteByte(0)
		encoded.WriteString(string(directory.Scope))
		encoded.WriteByte(0)
		encoded.WriteString(string(directory.Kind))
		encoded.WriteByte(0)
		if directory.Present {
			encoded.WriteString("present")
		} else {
			encoded.WriteString("absent")
		}
		encoded.WriteByte(0)
		encoded.WriteString(directory.SHA256)
		encoded.WriteByte('\n')
	}
	return task.ComputeSHA256([]byte(encoded.String()))
}

func inventoryError(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedProfile, fmt.Sprintf(format, arguments...))
}

func inventoryTooLarge(reason string) error {
	return fmt.Errorf("%w: %w: %s", ErrUnsupportedProfile, ErrInventoryTooLarge, reason)
}
