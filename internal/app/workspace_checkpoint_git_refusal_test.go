package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// Publication accepts only the persisted candidate bound to its request: an
// altered request digest or metadata fails the contract, and an altered stored
// payload or commit ID fails the native hash comparison, before any ref is
// observed or created. The unaltered candidate still publishes afterwards.
func TestNativeCheckpointPublicationRefusesAlteredCandidatesBeforePin(t *testing.T) {
	c := newCheckpointGit(t, "sha1")
	c.dirty(t, c.lineage.main)
	prepared := c.prepareOrdinary(t, c.lineage.main)
	headTree := c.lineage.gitIn(t, c.lineage.main, "rev-parse", "--verify", "HEAD^{tree}")
	cases := map[string]struct {
		alter func(*preparedCheckpoint)
		want  error
	}{
		"request digest": {alter: func(p *preparedCheckpoint) { p.requestSHA256 = task.ComputeSHA256([]byte("other request")) }, want: task.ErrIdentityMismatch},
		"tree metadata":  {alter: func(p *preparedCheckpoint) { p.candidate.TreeOID = headTree }, want: task.ErrIdentityMismatch},
		"stored payload": {alter: func(p *preparedCheckpoint) {
			p.candidate.CommitPayload = strings.Replace(p.candidate.CommitPayload, "\n\nDelegate checkpoint\n", "\n\nDelegate checkpoint altered\n", 1)
		}, want: workspace.ErrIdentityMismatch},
		"commit hash": {alter: func(p *preparedCheckpoint) { p.candidate.CommitOID = p.candidate.ObservedHEAD }, want: workspace.ErrIdentityMismatch},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			altered := prepared
			test.alter(&altered)
			err := c.lineage.git.PublishCheckpoint(c.lineage.scope.ctx, altered.request, altered.requestSHA256, altered.candidate)
			if !errors.Is(err, test.want) {
				t.Fatalf("publish altered candidate: %v, want %v", err, test.want)
			}
			c.requireAbsentRef(t, c.lineage.main, prepared.candidate.SnapshotRef)
		})
	}
	c.publish(t, prepared)
	c.requirePublishedCommit(t, c.lineage.main, prepared.candidate)
}

// The checkpoint pin is create-only through the shared owned-ref owner: an
// existing different direct ref is a conflict and a dangling symbolic ref is
// unreadable; both stay unchanged and no branch or HEAD moves.
func TestNativeCheckpointPublicationIsCreateOnly(t *testing.T) {
	c := newCheckpointGit(t, "sha1")
	before := c.observe(t, c.lineage.main)
	conflicting := c.prepareOrdinary(t, c.lineage.main)
	ref := conflicting.candidate.SnapshotRef
	c.lineage.gitIn(t, c.lineage.main, "update-ref", ref, conflicting.candidate.ObservedHEAD)
	err := c.lineage.git.PublishCheckpoint(c.lineage.scope.ctx, conflicting.request, conflicting.requestSHA256, conflicting.candidate)
	if !errors.Is(err, workspace.ErrRefConflict) {
		t.Fatalf("publish over a different direct ref: %v, want ErrRefConflict", err)
	}
	if got := c.lineage.gitIn(t, c.lineage.main, "rev-parse", "--verify", ref); got != conflicting.candidate.ObservedHEAD {
		t.Fatalf("conflicting ref now names %s", got)
	}
	symbolic := c.prepareOrdinary(t, c.lineage.main)
	target := c.danglingCheckpointRef(t, symbolic.candidate.SnapshotRef)
	err = c.lineage.git.PublishCheckpoint(c.lineage.scope.ctx, symbolic.request, symbolic.requestSHA256, symbolic.candidate)
	if !errors.Is(err, workspace.ErrRefUnreadable) {
		t.Fatalf("publish over a dangling symbolic ref: %v, want ErrRefUnreadable", err)
	}
	c.requireDanglingRef(t, symbolic.candidate.SnapshotRef, target)
	if after := c.observe(t, c.lineage.main); after != before {
		t.Fatalf("refused publication changed ordinary state: %+v, want %+v", after, before)
	}
}

// Removal deletes only the owned ref naming the recorded commit in a verified
// common directory. A conflicting or symbolic ref is refused unchanged, a
// missing common directory is never success, and the caller's canonical pinned
// identity check refuses a replaced repository.
func TestNativeCheckpointRemovalRefusesUnverifiableState(t *testing.T) {
	c := newCheckpointGit(t, "sha1")
	ctx := c.lineage.scope.ctx
	conflicting := c.prepareOrdinary(t, c.lineage.main)
	c.publish(t, conflicting)
	ref := conflicting.candidate.SnapshotRef
	c.lineage.gitIn(t, c.lineage.main, "update-ref", ref, conflicting.candidate.ObservedHEAD)
	if removed, err := c.lineage.git.RemoveCheckpoint(ctx, conflicting.request.RootID, conflicting.candidate); removed || !errors.Is(err, workspace.ErrRefConflict) {
		t.Fatalf("remove a conflicting ref = %v, %v; want ErrRefConflict", removed, err)
	}
	if got := c.lineage.gitIn(t, c.lineage.main, "rev-parse", "--verify", ref); got != conflicting.candidate.ObservedHEAD {
		t.Fatalf("conflicting ref now names %s", got)
	}
	symbolic := c.prepareOrdinary(t, c.lineage.main)
	target := c.danglingCheckpointRef(t, symbolic.candidate.SnapshotRef)
	if removed, err := c.lineage.git.RemoveCheckpoint(ctx, symbolic.request.RootID, symbolic.candidate); removed || !errors.Is(err, workspace.ErrRefUnreadable) {
		t.Fatalf("remove a dangling symbolic ref = %v, %v; want ErrRefUnreadable", removed, err)
	}
	c.requireDanglingRef(t, symbolic.candidate.SnapshotRef, target)
	published := c.prepareOrdinary(t, c.lineage.main)
	c.publish(t, published)
	c.requireMissingCommonDirectoryRefused(t, published)
	c.requirePublishedCommit(t, c.lineage.main, published.candidate)
	c.requireReplacedCommonDirectoryRefused(t, published.candidate.Target.Source)
}

func (c checkpointGit) requireMissingCommonDirectoryRefused(t *testing.T, prepared preparedCheckpoint) {
	t.Helper()
	moved := c.lineage.main + "-moved"
	if err := os.Rename(c.lineage.main, moved); err != nil {
		t.Fatal(err)
	}
	removed, err := c.lineage.git.RemoveCheckpoint(c.lineage.scope.ctx, prepared.request.RootID, prepared.candidate)
	if renameErr := os.Rename(moved, c.lineage.main); renameErr != nil {
		t.Fatal(renameErr)
	}
	if removed || err == nil {
		t.Fatalf("remove with a missing common directory = %v, %v; want a refusal", removed, err)
	}
}

// requireReplacedCommonDirectoryRefused replaces the repository at the same
// path with a new one of the same object format. Its Git-level identity is
// indistinguishable, so the pinned identity the caller verifies first refuses.
func (c checkpointGit) requireReplacedCommonDirectoryRefused(t *testing.T, source workspace.Source) {
	t.Helper()
	if err := workspacescore.VerifyRepositoryIdentity(source); err != nil {
		t.Fatalf("pinned identity of the original repository: %v", err)
	}
	if err := os.Rename(source.CommonDir, filepath.Join(c.lineage.root, "original.git")); err != nil {
		t.Fatal(err)
	}
	c.lineage.gitIn(t, c.lineage.main, "init", "--quiet", "--object-format="+source.ObjectFormat)
	if err := workspacescore.VerifyRepositoryIdentity(source); !errors.Is(err, workspace.ErrIdentity) {
		t.Fatalf("pinned identity of a replaced repository: %v, want ErrIdentity", err)
	}
}

// danglingCheckpointRef makes ref a symbolic ref to an absent owned name, so
// no real branch is ever a write target, and returns that target.
func (c checkpointGit) danglingCheckpointRef(t *testing.T, ref string) string {
	t.Helper()
	target := ref + "-missing-target"
	c.lineage.gitIn(t, c.lineage.main, "symbolic-ref", ref, target)
	return target
}

func (c checkpointGit) requireDanglingRef(t *testing.T, ref, target string) {
	t.Helper()
	if got := c.lineage.gitIn(t, c.lineage.main, "symbolic-ref", "--quiet", "--no-recurse", ref); got != target {
		t.Fatalf("symbolic ref %s now points to %q, want %s", ref, got, target)
	}
	if c.lineage.gitSucceeds(t, c.lineage.main, "show-ref", "--verify", "--quiet", target) {
		t.Fatalf("dangling target %s was created", target)
	}
}
