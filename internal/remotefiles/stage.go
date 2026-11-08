package remotefiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
)

// Stage owns bytes beneath a private caller-created root. The caller must hold
// its task/reference journal transaction across each method: the existing
// journal is the cross-process serialization authority. A manifest must be
// committed in that journal before opening the stage, and its commit receipt
// must be persisted only after Commit succeeds. No provider admission is done.
type Stage struct {
	root     *os.Root
	manifest Manifest
}

func OpenStage(root *os.Root, manifest Manifest) (*Stage, error) {
	if root == nil {
		return nil, errors.New("stage requires a rooted directory")
	}
	if err := ValidateManifest(manifest); err != nil {
		return nil, err
	}
	owned := Manifest{Entries: append([]Entry(nil), manifest.Entries...)}
	return &Stage{root: root, manifest: owned}, nil
}

func (s *Stage) entry(name string) (Entry, error) {
	for _, entry := range s.manifest.Entries {
		if entry.Path == name {
			return entry, nil
		}
	}
	return Entry{}, fmt.Errorf("file %q is not declared", name)
}

func stageNames(entry Entry) (string, string) {
	sum := sha256.Sum256([]byte(entry.Path))
	stem := hex.EncodeToString(sum[:]) + "-" + entry.SHA256
	return stem + ".part", stem + ".complete"
}

// Offset reports only a durable prefix (writes sync before acknowledging).
func (s *Stage) Offset(ctx context.Context, name string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	entry, err := s.entry(name)
	if err != nil {
		return 0, err
	}
	partial, complete := stageNames(entry)
	if _, err = s.root.Lstat(complete); err == nil {
		if err = s.recoverComplete(ctx, entry, partial, complete); err != nil {
			return 0, err
		}
		return entry.Size, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	f, err := openRegular(s.root, partial)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	info, statErr := f.Stat()
	err = errors.Join(statErr, barrierFile(f), f.Close())
	if err != nil {
		return 0, err
	}
	if info.Size() > entry.Size {
		return 0, fmt.Errorf("%w: oversized partial %q", ErrConflict, name)
	}
	return info.Size(), nil
}

func (s *Stage) WriteChunk(ctx context.Context, name string, offset int64, data []byte) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	entry, err := s.entry(name)
	if err != nil {
		return err
	}
	if len(data) > MaxChunkBytes || offset < 0 || offset > entry.Size || int64(len(data)) > entry.Size-offset {
		return fmt.Errorf("invalid chunk bounds for %q", name)
	}
	current, err := s.Offset(ctx, name)
	if err != nil {
		return err
	}
	if current != offset {
		return s.verifyRepeatedChunk(ctx, entry, offset, current, data)
	}
	partial, complete := stageNames(entry)
	if _, err = s.root.Lstat(complete); err == nil {
		return fmt.Errorf("%w: file is already committed", ErrConflict)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := s.openPartial(partial)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if err = writeSyncedChunk(f, data); err != nil {
		return err
	}
	return barrierRoot(s.root)
}

// Commit verifies every declared file before publishing complete byte objects.
// It is restartable; an error is never a complete-input receipt.
func (s *Stage) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, entry := range s.manifest.Entries {
		if err := s.commitEntry(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stage) openPartial(name string) (*os.File, error) {
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if !errors.Is(err, os.ErrExist) {
		return f, err
	}
	f, err = openRegular(s.root, name)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return openWritable(s.root, name)
}

func (s *Stage) commitEntry(ctx context.Context, entry Entry) error {
	partial, complete := stageNames(entry)
	if _, err := s.root.Lstat(complete); err == nil {
		return s.recoverComplete(ctx, entry, partial, complete)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if entry.Size == 0 {
		if err := s.WriteChunk(ctx, entry.Path, 0, nil); err != nil {
			return err
		}
	}
	if err := s.verify(ctx, entry, partial); err != nil {
		return err
	}
	if err := s.root.Link(partial, complete); err != nil {
		return err
	}
	if err := barrierRoot(s.root); err != nil {
		return err
	}
	if err := s.root.Remove(partial); err != nil {
		return err
	}
	return barrierRoot(s.root)
}

func (s *Stage) verify(ctx context.Context, entry Entry, name string) (resultErr error) {
	f, err := openRegular(s.root, name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	digest := sha256.New()
	n, err := copyContext(ctx, digest, io.LimitReader(f, entry.Size+1))
	if err != nil {
		return err
	}
	if n != entry.Size || hex.EncodeToString(digest.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("%w: length or digest mismatch for %q", ErrConflict, entry.Path)
	}
	return nil
}

// Materialize copies committed input bytes into an empty execution directory.
// The caller publishes the directory atomically only after success. It never
// links source bytes into the writable execution copy.
func (s *Stage) Materialize(ctx context.Context, destination *os.Root) error {
	for _, entry := range s.manifest.Entries {
		_, complete := stageNames(entry)
		if err := s.verify(ctx, entry, complete); err != nil {
			return err
		}
		if err := mkdirParents(destination, entry.Path); err != nil {
			return err
		}
		source, err := openRegular(s.root, complete)
		if err != nil {
			return err
		}
		writeErr := publishCopy(ctx, destination, entry, source)
		if err = errors.Join(writeErr, source.Close()); err != nil {
			return err
		}
	}
	return barrierRoot(destination)
}

func mkdirParents(root *os.Root, name string) error {
	if err := ValidatePath(name); err != nil {
		return err
	}
	parent := path.Dir(name)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	return validateParents(root, name)
}

func barrierRoot(root *os.Root) (resultErr error) {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	return f.Sync()
}

// recoverComplete settles interruption between publishing the hard link and
// removing the partial name. Both names must refer to the same owned file.
func (s *Stage) recoverComplete(ctx context.Context, entry Entry, partial, complete string) error {
	final, err := s.root.Lstat(complete)
	if err != nil {
		return err
	}
	if !final.Mode().IsRegular() {
		return fmt.Errorf("complete file must be regular")
	}
	old, err := s.root.Lstat(partial)
	if err == nil {
		if !os.SameFile(final, old) {
			return fmt.Errorf("%w: partial and complete disagree", ErrConflict)
		}
		if err = s.root.Remove(partial); err != nil {
			return err
		}
		if err = barrierRoot(s.root); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.verify(ctx, entry, complete)
}

func writeSyncedChunk(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return barrierFile(file)
}

// Lost chunk acknowledgments may replay the exact immutable range. A matching
// already-durable prefix is acknowledged without rewriting any bytes.
func (s *Stage) verifyRepeatedChunk(ctx context.Context, entry Entry, offset, current int64, data []byte) (resultErr error) {
	if current < offset+int64(len(data)) {
		return fmt.Errorf("%w: offset is %d, requested %d", ErrConflict, current, offset)
	}
	partial, complete := stageNames(entry)
	name := partial
	if _, err := s.root.Lstat(complete); err == nil {
		name = complete
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := openRegular(s.root, name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err = ctx.Err(); err != nil {
		return err
	}
	buffer := make([]byte, len(data))
	if _, err = file.ReadAt(buffer, offset); err != nil {
		return err
	}
	if !bytes.Equal(data, buffer) {
		return fmt.Errorf("%w: replayed chunk bytes differ", ErrConflict)
	}
	return nil
}
