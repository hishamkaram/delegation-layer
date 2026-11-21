package workspacefiles

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func directoryObservation(info os.FileInfo, path string) (task.DirectoryIdentity, error) {
	native, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || native.Ino == 0 {
		return task.DirectoryIdentity{}, failure(Changed, "scope_identity", nil)
	}
	return task.DirectoryIdentity{Path: path, Device: uint64(native.Dev), Inode: uint64(native.Ino)}, nil
}

func metadataVersion(info os.FileInfo) (string, error) {
	native, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", failure(Changed, "metadata", nil)
	}
	value := strconv.FormatUint(uint64(native.Dev), 10) + ":" + strconv.FormatUint(uint64(native.Ino), 10) + ":" +
		strconv.FormatUint(uint64(info.Mode()), 10) + ":" + strconv.FormatInt(info.Size(), 10) + ":" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + changeTime(native)
	return digest(value), nil
}

func textValue(value string) *string {
	if !utf8.ValidString(value) {
		return nil
	}
	return &value
}

func entryKind(mode os.FileMode) EntryKind {
	switch {
	case mode.IsRegular():
		return RegularFile
	case mode.IsDir():
		return Directory
	case mode&os.ModeSymlink != 0:
		return Symlink
	case mode&os.ModeNamedPipe != 0:
		return FIFO
	case mode&os.ModeSocket != 0:
		return Socket
	case mode&os.ModeDevice != 0:
		return Device
	default:
		return Other
	}
}

func makeEntry(path string, info os.FileInfo) (Entry, error) {
	version, err := metadataVersion(info)
	if err != nil {
		return Entry{}, err
	}
	name := filepath.Base(path)
	return Entry{
		Name: textValue(name), NameBase64: base64.StdEncoding.EncodeToString([]byte(name)),
		RelativePath: textValue(path), RelativePathBase64: base64.StdEncoding.EncodeToString([]byte(path)),
		Kind: entryKind(info.Mode()), Size: info.Size(), ModifiedAt: info.ModTime().UTC(), Version: version,
	}, nil
}

func requireVersion(expected, observed string) error {
	if expected == "" {
		return nil
	}
	if task.ValidateSHA256(expected) != nil {
		return failure(InvalidRequest, "version", nil)
	}
	if expected != observed {
		return failure(Changed, "version", nil)
	}
	return nil
}

func (s *selection) inspect(ctx context.Context, path, expectedVersion string) (*InspectResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify("inspect", err)
	}
	info, err := s.root.Lstat(path)
	if err != nil {
		return nil, classify("inspect", err)
	}
	entry, err := makeEntry(path, info)
	if err != nil {
		return nil, err
	}
	if err = requireVersion(expectedVersion, entry.Version); err != nil {
		return nil, err
	}
	result := &InspectResult{Entry: entry}
	if info.IsDir() {
		result.Git = s.inspectGit(path)
	}
	return result, nil
}
