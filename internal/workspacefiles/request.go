package workspacefiles

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func failure(code ErrorCode, stage string, cause error) error {
	return &Error{Code: code, Stage: stage, Retryable: code == Changed || code == Timeout || code == ResourceLimit || code == TempUnavailable, cause: cause}
}

func classify(stage string, err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	code := Changed
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = Timeout
	case errors.Is(err, fs.ErrNotExist):
		code = NotFound
	case errors.Is(err, fs.ErrPermission):
		code = PermissionDenied
	case errors.Is(err, unix.ENOTDIR):
		code = NotDirectory
	case errors.Is(err, unix.EMFILE), errors.Is(err, unix.ENFILE), errors.Is(err, unix.ENOMEM):
		code = ResourceLimit
	case errors.Is(err, task.ErrIdentityMismatch):
		code = Changed
	case rootEscape(err):
		// os.Root returns this private error for rooted symlink containment.
		code = OutsideScope
	}
	return failure(code, stage, err)
}

func rootEscape(err error) bool {
	var pathError *fs.PathError
	return errors.As(err, &pathError) && pathError.Err != nil && pathError.Err.Error() == "path escapes from parent"
}

func requestPath(request Request) (string, error) {
	path := request.RelativePath
	if request.RelativePathBase64 != "" {
		if path != "" || len(request.RelativePathBase64) > base64.StdEncoding.EncodedLen(4096) {
			return "", failure(InvalidRequest, "selector", nil)
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(request.RelativePathBase64)
		if err != nil {
			return "", failure(InvalidRequest, "selector", err)
		}
		path = string(decoded)
	} else if !utf8.ValidString(path) {
		return "", failure(InvalidRequest, "selector", nil)
	}
	if path == "" {
		path = "."
	}
	if len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", failure(InvalidRequest, "selector", nil)
	}
	if !filepath.IsLocal(path) {
		return "", failure(OutsideScope, "selector", nil)
	}
	// Preserve native symlink/parent traversal semantics; lexical cleaning can
	// silently select another file. os.Root enforces actual containment.
	return path, nil
}

func childPath(parent, name string) string {
	if parent == "." || parent == "" {
		return name
	}
	return parent + string(filepath.Separator) + name
}

func validateRequest(ctx context.Context, operation Operation, request Request) (string, error) {
	if ctx == nil {
		return "", failure(InvalidRequest, "context", nil)
	}
	if err := ctx.Err(); err != nil {
		return "", classify("context", err)
	}
	path, err := requestPath(request)
	if err != nil {
		return "", err
	}
	if request.Offset < 0 || request.Limit < 0 || request.Offset > math.MaxInt64-MaxReadBytes {
		return "", failure(InvalidRequest, "range", nil)
	}
	if len(request.Cursor) > 16384 {
		return "", failure(InvalidRequest, "selector", nil)
	}
	if err = validateScopeSelection(request.Scope); err != nil {
		return "", err
	}
	if request.ExpectedRoot != "" && task.ValidateSHA256(request.ExpectedRoot) != nil {
		return "", failure(InvalidRequest, "root_guard", nil)
	}
	if err = validateOptions(operation, request, path); err != nil {
		return "", err
	}
	return path, nil
}

func validateOptions(operation Operation, request Request, path string) error {
	switch operation {
	case List:
		if request.Limit > MaxEntries || request.Offset != 0 {
			return failure(InvalidRequest, "list_options", nil)
		}
	case Read:
		if request.Limit > MaxReadBytes || request.Cursor != "" {
			return failure(InvalidRequest, "read_options", nil)
		}
	case Inspect, Git, Scopes:
		if request.Limit != 0 || request.Offset != 0 || request.Cursor != "" {
			return failure(InvalidRequest, "observation_options", nil)
		}
	default:
		return failure(Unsupported, "operation", nil)
	}
	if operation == Scopes && (path != "." || request.ExpectedVersion != "") {
		return failure(InvalidRequest, "scopes_options", nil)
	}
	return nil
}

func validateScopeSelection(scope Scope) error {
	if len(scope.Directory) > 4096 || len(scope.ExpectedWorkspace) > 16384 {
		return failure(InvalidRequest, "selector", nil)
	}
	if scope.Kind == "" && (scope.Directory != "" || scope.ExpectedWorkspace != "") {
		return failure(InvalidRequest, "scope", nil)
	}
	return nil
}
