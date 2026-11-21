package workspacefiles

import (
	"os"
	"path/filepath"
	"testing"
)

func gitDirectories(t *testing.T, base string) {
	t.Helper()
	for _, path := range []string{"objects", "refs"} {
		must(t, os.MkdirAll(filepath.Join(base, path), 0o700))
	}
	write(t, filepath.Join(base, "HEAD"), []byte("ref: refs/heads/main\n"))
}

func TestStructuralGitDoesNotOverclaimMarkersAndHandlesRepositoryLayouts(t *testing.T) {
	f := newFixture(t)
	gitDirectories(t, filepath.Join(f.path, "standard", ".git"))
	gitDirectories(t, filepath.Join(f.path, "bare"))
	gitDirectories(t, filepath.Join(f.path, "nested", "child", ".git"))
	must(t, os.Mkdir(filepath.Join(f.path, "linked"), 0o700))
	write(t, filepath.Join(f.path, "linked", ".git"), []byte("gitdir: ../standard/.git/worktrees/linked\n"))
	linkedMetadata := filepath.Join(f.path, "standard", ".git", "worktrees", "linked")
	must(t, os.MkdirAll(linkedMetadata, 0o700))
	write(t, filepath.Join(linkedMetadata, "commondir"), []byte("../..\n"))
	write(t, filepath.Join(linkedMetadata, "HEAD"), []byte("ref: refs/heads/unborn\n"))
	must(t, os.Mkdir(filepath.Join(f.path, "malformed"), 0o700))
	write(t, filepath.Join(f.path, "malformed", ".git"), []byte("not a git directory\n"))
	must(t, os.Mkdir(filepath.Join(f.path, "external"), 0o700))
	write(t, filepath.Join(f.path, "external", ".git"), []byte("gitdir: ../../outside\n"))
	gitDirectories(t, filepath.Join(f.path, "bad-head", ".git"))
	write(t, filepath.Join(f.path, "bad-head", ".git", "HEAD"), []byte("not a reference"))
	cases := []struct {
		path   string
		status GitStatus
		kind   GitKind
		shape  string
	}{
		{".", GitNotRepository, "", ""},
		{"standard", GitCandidate, GitWorktree, "symbolic"},
		{"bare", GitCandidate, GitBare, "symbolic"},
		{"linked", GitCandidate, GitLinkedWorktree, "symbolic"},
		{"nested", GitNotRepository, "", ""},
		{"nested/child", GitCandidate, GitWorktree, "symbolic"},
		{"malformed", GitFault, GitLinkedWorktree, ""},
		{"external", GitPartial, GitLinkedWorktree, ""},
		{"bad-head", GitFault, GitWorktree, "malformed"},
	}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			request := f.request
			request.RelativePath = test.path
			result := execute(t, f, Git, request).Inspect.Git
			if result == nil || result.Status != test.status || result.Kind != test.kind || result.HEADShape != test.shape {
				t.Fatalf("classification = %+v, want %s %s %s", result, test.status, test.kind, test.shape)
			}
		})
	}
	// A list marker says candidate even where explicit structural inspection
	// finds malformed bytes. No Git CLI or config/hook reader runs in either.
	page := execute(t, f, List, f.request).List
	for _, entry := range page.Entries {
		if entry.Name != nil && *entry.Name == "malformed" && (entry.Git == nil || entry.Git.Status != GitCandidate) {
			t.Fatal("list marker was advertised as a validated repository")
		}
	}
}

func TestGitExternalCommonAndAbsoluteInternalMetadataRespectTheScope(t *testing.T) {
	f := newFixture(t)
	must(t, os.Mkdir(filepath.Join(f.path, "linked"), 0o700))
	metadata := filepath.Join(f.path, "metadata")
	gitDirectories(t, metadata)
	write(t, filepath.Join(f.path, "linked", ".git"), []byte("gitdir: "+metadata+"\n"))
	request := f.request
	request.RelativePath = "linked"
	if result := execute(t, f, Git, request).Inspect.Git; result.Status != GitCandidate || result.HEADShape != "symbolic" {
		t.Fatalf("internal absolute metadata not observed: %+v", result)
	}
	write(t, filepath.Join(metadata, "commondir"), []byte("../../external\n"))
	if result := execute(t, f, Git, request).Inspect.Git; result.Status != GitPartial || result.Reason != "external_common_metadata" {
		t.Fatalf("external commondir became confirmed repository: %+v", result)
	}
}
