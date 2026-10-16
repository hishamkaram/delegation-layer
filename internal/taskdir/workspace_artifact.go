package taskdir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const workspaceArtifactNameLimit = 64

// VerifyWorkspaceArtifact validates one exact workspace artifact through the
// store's pinned root and streams its content into a bounded digest. It is
// shared by readers and the forget owner so structural and content checks have
// one implementation.
func (s *Store) VerifyWorkspaceArtifact(ctx context.Context, operationID, name string, length int64, digest string) error {
	path, err := s.workspaceArtifactPath(operationID, name, length, digest)
	if err != nil {
		return err
	}
	return s.verifyWorkspaceArtifact(ctx, path, length, digest)
}

// RemoveWorkspaceArtifact removes only an exact inventory entry through the
// pinned store. The workspace owner must first commit its forget intent and
// prove all relevant native lifetimes closed. Provider evidence is out of scope.
func (s *Store) RemoveWorkspaceArtifact(ctx context.Context, operationID, name string, length int64, digest string) (resultErr error) {
	path, err := s.workspaceArtifactPath(operationID, name, length, digest)
	if err != nil {
		return err
	}
	if err = s.verifyWorkspaceArtifact(ctx, path, length, digest); err != nil {
		if !workspaceArtifactAbsent(err) {
			return err
		}
		if ctx == nil {
			return context.Canceled
		}
		return ctx.Err()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	relative, err := s.relative(path)
	if err != nil {
		return err
	}
	if err = s.rootHandle.Remove(relative); err != nil && !workspaceArtifactAbsent(err) {
		return err
	}
	return s.barrierDir(filepath.Dir(path))
}

type workspaceArtifactJoinedError interface {
	Unwrap() []error
}

func workspaceArtifactAbsent(err error) bool {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return false
	}
	var joined workspaceArtifactJoinedError
	if errors.As(err, &joined) {
		return false
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.ENOENT
}

func (s *Store) workspaceArtifactPath(operationID, name string, length int64, digest string) (string, error) {
	if s == nil || s.rootHandle == nil {
		return "", task.ErrInvalidPermit
	}
	if err := errors.Join(task.ValidateTaskID(operationID), task.ValidateSHA256(digest)); err != nil {
		return "", err
	}
	if name == "" || len(name) > workspaceArtifactNameLimit || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") || length < 0 || length > task.MaxWorkspaceArtifactSize {
		return "", task.ErrEvidenceFault
	}
	return filepath.Join(WorkspaceArtifactDirectory(s.Root, operationID), name), nil
}

func (s *Store) verifyWorkspaceArtifact(ctx context.Context, path string, length int64, expectedDigest string) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := s.openFile(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	reader := contextReader{ctx: ctx, reader: file}
	digest := sha256.New()
	buffer := make([]byte, 32*1024)
	read, readErr := io.CopyBuffer(digest, io.LimitReader(reader, length+1), buffer)
	closeErr := file.Close()
	if err = errors.Join(readErr, closeErr, ctx.Err()); err != nil {
		return err
	}
	if read != length || hex.EncodeToString(digest.Sum(nil)) != expectedDigest {
		return task.ErrEvidenceFault
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	read, err := r.reader.Read(buffer)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return read, contextErr
	}
	return read, err
}
