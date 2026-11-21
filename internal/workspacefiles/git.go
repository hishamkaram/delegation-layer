package workspacefiles

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
)

// gitMarker is deliberately structural and performs no Git discovery upward.
// A malformed marker still remains a candidate for explicit inspection.
func (s *selection) gitMarker(path string) *GitResult {
	info, err := s.root.Lstat(childPath(path, ".git"))
	if err == nil {
		result := &GitResult{Status: GitCandidate, Kind: GitWorktree, Marker: "dot_git", Reason: "structural_only"}
		if info.Mode().IsRegular() {
			result.Kind = GitLinkedWorktree
		}
		return result
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return &GitResult{Status: GitFault, Reason: "marker_unreadable"}
	}
	return s.bareMarker(path)
}

func (s *selection) bareMarker(path string) *GitResult {
	for _, name := range []string{"HEAD", "objects", "refs"} {
		info, err := s.root.Stat(childPath(path, name))
		if errors.Is(err, fs.ErrNotExist) {
			return &GitResult{Status: GitNotRepository}
		}
		if err != nil {
			status := GitFault
			var typed *Error
			if errors.As(classify("git_metadata", err), &typed) && typed.Code == OutsideScope {
				status = GitPartial
			}
			return &GitResult{Status: status, Kind: GitBare, Reason: "bare_metadata_unavailable"}
		}
		if name == "HEAD" && !info.Mode().IsRegular() || name != "HEAD" && !info.IsDir() {
			return &GitResult{Status: GitNotRepository}
		}
	}
	return &GitResult{Status: GitCandidate, Kind: GitBare, Marker: "bare_layout", Reason: "structural_only"}
}

func (s *selection) isDirectory(path string) bool {
	info, err := s.root.Stat(path)
	return err == nil && info.IsDir()
}

func (s *selection) inspectGit(path string) *GitResult {
	result := s.gitMarker(path)
	if result.Status != GitCandidate {
		return result
	}
	metadata := path
	if result.Kind != GitBare {
		metadata = childPath(path, ".git")
		info, err := s.root.Stat(metadata)
		if err != nil {
			result.Status, result.Reason = GitPartial, "metadata_unavailable_in_scope"
			return result
		}
		if info.Mode().IsRegular() {
			return s.inspectLinkedGit(path, metadata, result)
		}
		if !info.IsDir() {
			result.Status, result.Reason = GitFault, "invalid_marker_kind"
			return result
		}
	}
	return s.inspectGitMetadata(metadata, result)
}

func (s *selection) inspectLinkedGit(path, marker string, result *GitResult) *GitResult {
	data, err := s.smallMetadata(marker)
	if err != nil {
		result.Status, result.Reason = GitFault, "marker_unreadable"
		return result
	}
	value := strings.TrimSuffix(string(data), "\n")
	if !strings.HasPrefix(value, "gitdir: ") || strings.ContainsAny(value, "\x00\r\n") {
		result.Status, result.Reason = GitFault, "malformed_gitdir_marker"
		return result
	}
	metadata, ok := s.metadataPath(path, strings.TrimPrefix(value, "gitdir: "))
	result.Kind = GitLinkedWorktree
	if !ok {
		result.Status, result.Reason = GitPartial, "external_metadata"
		return result
	}
	return s.inspectGitMetadata(metadata, result)
}

func (s *selection) metadataPath(base, target string) (string, bool) {
	if target == "" {
		return "", false
	}
	path := childPath(base, target)
	if filepath.IsAbs(target) {
		prefix := strings.TrimSuffix(s.root.Name(), string(filepath.Separator)) + string(filepath.Separator)
		if target == s.root.Name() {
			path = "."
		} else if strings.HasPrefix(target, prefix) {
			path = strings.TrimPrefix(target, prefix)
		} else {
			return "", false
		}
	}
	return path, filepath.IsLocal(path)
}

func (s *selection) inspectGitMetadata(metadata string, result *GitResult) *GitResult {
	result.MetadataPathBase64 = base64.StdEncoding.EncodeToString([]byte(metadata))
	if !s.isDirectory(metadata) {
		result.Status, result.Reason = GitPartial, "metadata_unavailable_in_scope"
		return result
	}
	if common, err := s.smallMetadata(childPath(metadata, "commondir")); err == nil {
		commonPath, ok := s.metadataPath(metadata, strings.TrimSuffix(string(common), "\n"))
		if !ok || !s.isDirectory(commonPath) {
			result.Status, result.Reason = GitPartial, "external_common_metadata"
			return result
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		result.Status, result.Reason = GitPartial, "common_metadata_unavailable"
		return result
	}
	head, err := s.smallMetadata(childPath(metadata, "HEAD"))
	if err != nil {
		result.Status, result.Reason = GitFault, "head_unreadable"
		var typed *Error
		if errors.As(err, &typed) && typed.Code == OutsideScope {
			result.Status, result.Reason = GitPartial, "external_head_metadata"
		}
		return result
	}
	result.HEAD = textValue(string(head))
	result.HEADBase64 = base64.StdEncoding.EncodeToString(head)
	result.HEADShape = headShape(string(head))
	if result.HEADShape == "malformed" {
		result.Status, result.Reason = GitFault, "malformed_head"
	}
	return result
}

func headShape(head string) string {
	value := strings.TrimSuffix(head, "\n")
	if strings.ContainsAny(value, "\x00\r\n") {
		return "malformed"
	}
	if strings.HasPrefix(value, "ref: refs/") && len(value) > len("ref: refs/") {
		return "symbolic"
	}
	if len(value) == 40 || len(value) == 64 {
		if _, err := hex.DecodeString(value); err == nil {
			return "detached"
		}
	}
	return "malformed"
}

// smallMetadata reads only the fixed Git marker/HEAD/commondir metadata
// requested by structural inspection, with a hard per-file bound.
func (s *selection) smallMetadata(path string) (data []byte, resultErr error) {
	file, err := s.openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, classify("metadata_close", file.Close())) }()
	data, err = io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, classify("metadata_read", err)
	}
	if len(data) > 4096 {
		return nil, failure(ResourceLimit, "metadata_read", nil)
	}
	return data, nil
}
