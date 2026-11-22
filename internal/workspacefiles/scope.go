package workspacefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type selection struct {
	root *os.Root
	id   string
}

func (s *selection) Close() error { return s.root.Close() }

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func openScope(ctx context.Context, scope Scope) (*selection, error) {
	path, expected, err := resolveScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, classify("scope_open", err)
	}
	if err = validateScopeRoot(root, path, expected); err != nil {
		return nil, errors.Join(err, classify("scope_close", root.Close()))
	}
	return &selection{root: root, id: digest(string(scope.Kind) + "\x00" + path + "\x00" + scope.ExpectedWorkspace)}, nil
}

func resolveScope(ctx context.Context, scope Scope) (string, task.DirectoryIdentity, error) {
	switch scope.Kind {
	case Workspace:
		return resolveWorkspace(ctx, scope)
	case SharedTemp, UserTemp:
		if scope.ExpectedWorkspace != "" {
			return "", task.DirectoryIdentity{}, failure(InvalidRequest, "temp_scope", nil)
		}
		scopes, err := nativeTempScopes(ctx)
		if err != nil {
			return "", task.DirectoryIdentity{}, err
		}
		for _, observed := range scopes {
			if observed.Kind == scope.Kind {
				if scope.Directory != "" && scope.Directory != observed.Directory {
					return "", task.DirectoryIdentity{}, failure(OutsideScope, "temp_scope", nil)
				}
				return observed.Directory, task.DirectoryIdentity{}, nil
			}
		}
		return "", task.DirectoryIdentity{}, failure(TempUnavailable, "temp_scope", nil)
	default:
		return "", task.DirectoryIdentity{}, failure(InvalidRequest, "scope", nil)
	}
}

func resolveWorkspace(ctx context.Context, scope Scope) (string, task.DirectoryIdentity, error) {
	if scope.ExpectedWorkspace == "" {
		return "", task.DirectoryIdentity{}, failure(InvalidRequest, "workspace_guard", nil)
	}
	identity, err := taskdir.ResolveProjectTokenContext(ctx, scope.ExpectedWorkspace)
	if err != nil {
		return "", task.DirectoryIdentity{}, classify("workspace_guard", err)
	}
	if scope.Directory != "" && scope.Directory != identity.Path {
		return "", task.DirectoryIdentity{}, failure(OutsideScope, "workspace_guard", nil)
	}
	canonical, err := filepath.EvalSymlinks(identity.Path)
	if err != nil {
		return "", task.DirectoryIdentity{}, classify("workspace_guard", err)
	}
	if canonical != identity.Path {
		return "", task.DirectoryIdentity{}, failure(Changed, "workspace_guard", nil)
	}
	return canonical, identity, nil
}

func validateScopeRoot(root *os.Root, path string, expected task.DirectoryIdentity) (resultErr error) {
	file, err := root.Open(".")
	if err != nil {
		return classify("scope_identity", err)
	}
	defer func() { resultErr = errors.Join(resultErr, classify("scope_close", file.Close())) }()
	info, err := file.Stat()
	if err != nil {
		return classify("scope_identity", err)
	}
	visible, err := os.Stat(path)
	if err != nil {
		return classify("scope_identity", err)
	}
	if !os.SameFile(info, visible) {
		return failure(Changed, "scope_identity", nil)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return classify("scope_identity", err)
	}
	if canonical != path {
		return failure(Changed, "scope_identity", nil)
	}
	if expected.Path != "" {
		actual, err := directoryObservation(info, path)
		if err != nil {
			return err
		}
		if !expected.MatchesObservation(actual) {
			return failure(Changed, "workspace_guard", nil)
		}
	}
	return nil
}

func observeScopes(ctx context.Context, scope Scope) (*ScopesResult, error) {
	scopes, err := nativeTempScopes(ctx)
	if err != nil {
		return nil, err
	}
	if scope.Kind != "" {
		if scope.Kind != Workspace {
			return nil, failure(InvalidRequest, "scopes", nil)
		}
		selected, err := openScope(ctx, scope)
		if err != nil {
			return nil, err
		}
		workspace := ObservedScope{ID: selected.id, Kind: Workspace, Name: "Approved workspace", Directory: selected.root.Name()}
		if err = selected.Close(); err != nil {
			return nil, classify("scope_close", err)
		}
		scopes = append([]ObservedScope{workspace}, scopes...)
	}
	return &ScopesResult{Scopes: scopes}, nil
}

func canonicalTempScopes(paths []ObservedScope) ([]ObservedScope, error) {
	result := make([]ObservedScope, 0, len(paths))
	for _, scope := range paths {
		canonical, err := filepath.EvalSymlinks(scope.Directory)
		if err != nil || !filepath.IsAbs(canonical) {
			return nil, failure(TempUnavailable, "temp_lookup", err)
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			return nil, failure(TempUnavailable, "temp_lookup", err)
		}
		duplicate := false
		for _, existing := range result {
			duplicate = duplicate || existing.Directory == canonical
		}
		if !duplicate {
			scope.Directory = canonical
			scope.ID = digest(string(scope.Kind) + "\x00" + canonical + "\x00")
			result = append(result, scope)
		}
	}
	return result, nil
}
