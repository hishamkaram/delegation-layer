// Package remotefiles implements declared workspace byte transfer. The caller
// owns task admission, lifecycle, journal persistence and transfer serialization.
package remotefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/filepaths"
)

const (
	MaxEntries          = filepaths.MaxEntries
	MaxTotalBytes int64 = 1 << 30
	MaxFileBytes  int64 = 256 << 20
	MaxChunkBytes       = 64 << 10
)

var ErrConflict = errors.New("remote file identity conflict")

type Entry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable,omitempty"`
}

type Manifest struct {
	Entries []Entry `json:"entries"`
}

// ValidateManifest checks bounded file descriptors and their shared path set.
func ValidateManifest(manifest Manifest) error {
	if len(manifest.Entries) > MaxEntries {
		return fmt.Errorf("manifest exceeds %d entries", MaxEntries)
	}
	names := make([]string, 0, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		names = append(names, entry.Path)
	}
	if err := ValidateSelectors(names); err != nil {
		return err
	}
	var total int64
	for _, entry := range manifest.Entries {
		digest, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(digest) != sha256.Size || strings.ToLower(entry.SHA256) != entry.SHA256 {
			return fmt.Errorf("invalid SHA256 for %q", entry.Path)
		}
		if entry.Size < 0 || entry.Size > MaxFileBytes {
			return fmt.Errorf("invalid size for %q", entry.Path)
		}
		total += entry.Size
		if total > MaxTotalBytes {
			return fmt.Errorf("manifest exceeds %d bytes", MaxTotalBytes)
		}
	}

	return nil
}

// ManifestSHA256 normalizes entry order before hashing. It does not mutate the
// caller's manifest. The hash is source-input evidence, not executable identity.
func ManifestSHA256(manifest Manifest) (string, error) {
	if err := ValidateManifest(manifest); err != nil {
		return "", err
	}
	entries := append([]Entry(nil), manifest.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	data, err := json.Marshal(Manifest{Entries: entries})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func BuildManifest(ctx context.Context, root *os.Root, names []string) (Manifest, error) {
	result := Manifest{Entries: make([]Entry, 0, len(names))}
	if len(names) > MaxEntries {
		return Manifest{}, fmt.Errorf("manifest exceeds %d entries", MaxEntries)
	}
	for _, name := range names {
		entry, err := describeFile(ctx, root, name)
		if err != nil {
			return Manifest{}, err
		}
		result.Entries = append(result.Entries, entry)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, ValidateManifest(result)
}

func describeFile(ctx context.Context, root *os.Root, name string) (entry Entry, resultErr error) {
	return describeFileWithLimit(ctx, root, name, MaxFileBytes)
}

func describeFileWithLimit(ctx context.Context, root *os.Root, name string, remaining int64) (entry Entry, resultErr error) {
	f, err := openRegular(root, name)
	if err != nil {
		return entry, err
	}
	defer func() { resultErr = errors.Join(resultErr, captureIO(f.Close())) }()
	info, err := f.Stat()
	if err != nil {
		return entry, err
	}
	if info.Size() > MaxFileBytes {
		return entry, invalidSource("too_large", fmt.Errorf("file %q exceeds %d bytes", name, MaxFileBytes))
	}
	if info.Size() > remaining {
		return entry, invalidSource("aggregate_byte_limit", fmt.Errorf("file %q exceeds remaining capture bytes", name))
	}
	digest := sha256.New()
	count, err := copyContext(ctx, digest, io.LimitReader(f, min(MaxFileBytes, remaining)+1))
	if err != nil {
		return entry, err
	}
	if count > MaxFileBytes {
		return entry, invalidSource("too_large", fmt.Errorf("file %q grew beyond file limit", name))
	}
	if count > remaining {
		return entry, invalidSource("aggregate_byte_limit", fmt.Errorf("file %q grew beyond remaining capture bytes", name))
	}
	if count != info.Size() {
		return entry, invalidSource("changed", fmt.Errorf("file changed while reading %q", name))
	}
	final, err := f.Stat()
	if err != nil {
		return entry, err
	}
	if final.Size() != info.Size() || final.ModTime() != info.ModTime() {
		return entry, invalidSource("changed", fmt.Errorf("file changed while reading %q", name))
	}
	return Entry{Path: name, Size: count, SHA256: hex.EncodeToString(digest.Sum(nil)), Executable: info.Mode().Perm()&0o111 != 0}, nil
}

func openRegular(root *os.Root, name string) (*os.File, error) {
	if err := validateParents(root, name); err != nil {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, invalidSource("not_regular", fmt.Errorf("path %q must be a regular file", name))
	}
	if err = singleLink(before); err != nil {
		return nil, invalidSource("hard_link", fmt.Errorf("path %q: %w", name, err))
	}
	f, err := openReadOnly(root, name)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err == nil {
		err = validateOpenedRegular(name, before, after)
	}
	if err != nil {
		return nil, errors.Join(err, captureIO(f.Close()))
	}
	return f, nil
}

// The opened descriptor, rather than an earlier path observation, owns the
// authority to read. Recheck every source constraint after the native open.
func validateOpenedRegular(name string, before, after os.FileInfo) error {
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return invalidSource("changed", fmt.Errorf("file replaced while opening %q", name))
	}
	if err := singleLink(after); err != nil {
		return invalidSource("hard_link", fmt.Errorf("path %q: %w", name, err))
	}
	return nil
}

func validateParents(root *os.Root, name string) error {
	if err := ValidatePath(name); err != nil {
		return err
	}
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		info, err := root.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return invalidSource("not_regular", fmt.Errorf("parent %q must be a directory without symlinks", parent))
		}
	}
	return nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	var total int64
	buffer := make([]byte, MaxChunkBytes)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			written, err := dst.Write(buffer[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// ValidatePath accepts canonical relative workspace paths and excludes Git's
// mutable configuration/objects. Credentials and installed programs must never
// be selected by the calling input contract.
func ValidatePath(name string) error { return filepaths.ValidatePath(name) }
