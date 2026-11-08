package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// These cases run the public workspace.Git checkpoint ports over native Git
// through the nativeLineage launch boundary. They prove the Git preparation,
// publication and removal ports only; they are not a coordinator, journal,
// Pueue-worker or provider composition.

// checkpointGit is one native lineage with a private scratch directory
// disjoint from every repository and checkout.
type checkpointGit struct {
	lineage nativeLineage
	scratch string
}

// preparedCheckpoint is one candidate with the creation request and digest it
// binds to, as the journal would persist them.
type preparedCheckpoint struct {
	candidate     workspace.CheckpointCandidate
	request       workspace.OperationRequest
	requestSHA256 string
}

// checkoutState is the ordinary Git state no checkpoint port may change: the
// HEAD object, whether HEAD is attached, and the exact real-index bytes.
type checkoutState struct {
	head        string
	attached    bool
	indexSHA256 string
}

func newCheckpointGit(t *testing.T, objectFormat string) checkpointGit {
	t.Helper()
	lineage := newNativeLineageFormat(t, objectFormat)
	scratch := filepath.Join(lineage.root, "checkpoint-scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	return checkpointGit{lineage: lineage, scratch: scratch}
}

func (c checkpointGit) request(t *testing.T, selection *workspace.CheckpointSelection, workspaceID string) (workspace.OperationRequest, string) {
	t.Helper()
	operationID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: c.lineage.store.RootID, OperationID: operationID, WorkspaceID: workspaceID,
		Kind: workspace.OperationCheckpoint, Checkpoint: selection,
	}
	return request, task.ComputeSHA256([]byte("checkpoint " + operationID))
}

// prepareOrdinary resolves cwd, pins its common directory through the
// source directory's actual identity, and prepares the candidate.
func (c checkpointGit) prepareOrdinary(t *testing.T, cwd string) preparedCheckpoint {
	t.Helper()
	ctx := c.lineage.scope.ctx
	target, err := c.lineage.git.ResolveOrdinaryCheckpointTarget(ctx, cwd)
	if err != nil {
		t.Fatalf("resolve ordinary checkpoint target: %v", err)
	}
	commonIdentity, err := workspacescore.DirectoryIdentity(target.Source.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	target.Source = target.Source.WithCommonDirIdentity(commonIdentity)
	identity, err := workspacescore.DirectoryIdentity(cwd)
	if err != nil {
		t.Fatal(err)
	}
	request, digest := c.request(t, &workspace.CheckpointSelection{Kind: workspace.CheckpointOrdinary, Cwd: cwd, CwdIdentity: &identity}, "")
	candidate, err := c.lineage.git.PrepareOrdinaryCheckpoint(ctx, request, digest, target, c.scratch)
	if err != nil {
		t.Fatalf("prepare ordinary checkpoint: %v", err)
	}
	c.requireScratchEmpty(t)
	return preparedCheckpoint{candidate: candidate, request: request, requestSHA256: digest}
}

func (c checkpointGit) publish(t *testing.T, prepared preparedCheckpoint) {
	t.Helper()
	if err := c.lineage.git.PublishCheckpoint(c.lineage.scope.ctx, prepared.request, prepared.requestSHA256, prepared.candidate); err != nil {
		t.Fatalf("publish checkpoint: %v", err)
	}
}

func (c checkpointGit) requireScratchEmpty(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(c.scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("checkpoint preparation left private files: %v", entries)
	}
}

func (c checkpointGit) observe(t *testing.T, directory string) checkoutState {
	t.Helper()
	index := c.lineage.gitIn(t, directory, "rev-parse", "--path-format=absolute", "--git-path", "index")
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	return checkoutState{
		head:        c.lineage.gitIn(t, directory, "rev-parse", "--verify", "HEAD"),
		attached:    c.lineage.gitSucceeds(t, directory, "symbolic-ref", "--quiet", "HEAD"),
		indexSHA256: task.ComputeSHA256(data),
	}
}

// dirty leaves a modified tracked file, a staged new file and an untracked
// file below directory, so the full tree differs from HEAD and the index.
func (c checkpointGit) dirty(t *testing.T, directory string) {
	t.Helper()
	writeLineageFile(t, filepath.Join(directory, "tracked.txt"), "checkpoint edit\n")
	writeLineageFile(t, filepath.Join(directory, "staged.txt"), "staged\n")
	c.lineage.gitIn(t, directory, "add", "staged.txt")
	writeLineageFile(t, filepath.Join(directory, "untracked.txt"), "untracked\n")
}

// requirePublishedCommit reads the published checkpoint back natively: the
// owned ref names the commit, whose stored object is exactly the candidate
// payload with the full tree and the observed HEAD as its sole parent.
func (c checkpointGit) requirePublishedCommit(t *testing.T, directory string, candidate workspace.CheckpointCandidate) {
	t.Helper()
	if got := c.lineage.gitIn(t, directory, "rev-parse", "--verify", candidate.SnapshotRef); got != candidate.CommitOID {
		t.Fatalf("checkpoint ref names %s, want %s", got, candidate.CommitOID)
	}
	if c.lineage.gitSucceeds(t, directory, "symbolic-ref", "--quiet", "--no-recurse", candidate.SnapshotRef) {
		t.Fatal("checkpoint ref is symbolic")
	}
	if got := c.lineage.gitIn(t, directory, "cat-file", "commit", candidate.CommitOID); got+"\n" != candidate.CommitPayload {
		t.Fatalf("stored commit object = %q, want the exact candidate payload %q", got, candidate.CommitPayload)
	}
	if got := c.lineage.gitIn(t, directory, "rev-parse", "--verify", candidate.CommitOID+"^{tree}"); got != candidate.TreeOID {
		t.Fatalf("checkpoint tree = %s, want %s", got, candidate.TreeOID)
	}
	if parents := c.lineage.gitIn(t, directory, "rev-parse", candidate.CommitOID+"^@"); parents != candidate.ObservedHEAD {
		t.Fatalf("checkpoint parents = %q, want only observed HEAD %s", parents, candidate.ObservedHEAD)
	}
}

// requireTreeContent checks file content in the checkpoint commit by path.
func (c checkpointGit) requireTreeContent(t *testing.T, directory, commit string, files map[string]string) {
	t.Helper()
	for path, want := range files {
		if got := c.lineage.gitIn(t, directory, "show", commit+":"+path); got+"\n" != want {
			t.Errorf("checkpoint %s = %q, want %q", path, got, want)
		}
	}
}

func (c checkpointGit) requireAbsentRef(t *testing.T, directory, ref string) {
	t.Helper()
	if c.lineage.gitSucceeds(t, directory, "symbolic-ref", "--quiet", "--no-recurse", ref) ||
		c.lineage.gitSucceeds(t, directory, "show-ref", "--verify", "--quiet", ref) {
		t.Fatalf("checkpoint ref %s exists", ref)
	}
}

// commitSubdirectory commits sub/nested.txt in the main repository, so a
// selection below the checkout root maps to a tracked subdirectory.
func (c checkpointGit) commitSubdirectory(t *testing.T) string {
	t.Helper()
	sub := filepath.Join(c.lineage.main, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLineageFile(t, filepath.Join(sub, "nested.txt"), "nested\n")
	c.lineage.gitIn(t, c.lineage.main, "add", "sub/nested.txt")
	return c.lineage.commit(t, c.lineage.main, "subdirectory")
}

// An ordinary main or detached linked worktree, selected below its root, is
// its own source. Preparation binds the exact payload through native Git in
// both object formats without writing the commit or ref; publication writes
// that payload and pins it; HEAD, attachment and the real index never change;
// removal still works after the linked source checkout is gone.
func TestNativeCheckpointOrdinaryTargetsBindExactCommit(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format+"/main", func(t *testing.T) {
			c := newCheckpointGit(t, format)
			c.commitSubdirectory(t)
			c.requireOrdinaryCheckpoint(t, c.lineage.main, false)
		})
		t.Run(format+"/linked detached", func(t *testing.T) {
			c := newCheckpointGit(t, format)
			c.commitSubdirectory(t)
			c.requireOrdinaryCheckpoint(t, c.lineage.linkedSource(t, filepath.Join(c.lineage.root, "linked")), true)
		})
	}
}

func (c checkpointGit) requireOrdinaryCheckpoint(t *testing.T, root string, linked bool) {
	t.Helper()
	ctx := c.lineage.scope.ctx
	c.dirty(t, root)
	before := c.observe(t, root)
	prepared := c.prepareOrdinary(t, filepath.Join(root, "sub"))
	candidate := prepared.candidate
	c.requireOrdinaryTarget(t, root, candidate.Target)
	if candidate.ObservedHEAD != before.head || candidate.Target.Source.BaseOID != before.head {
		t.Fatalf("ordinary candidate HEAD/base = %s/%s, want %s", candidate.ObservedHEAD, candidate.Target.Source.BaseOID, before.head)
	}
	if c.lineage.gitSucceeds(t, root, "cat-file", "-e", candidate.CommitOID) {
		t.Fatal("preparation wrote the checkpoint commit object")
	}
	c.requireAbsentRef(t, root, candidate.SnapshotRef)
	c.publish(t, prepared)
	c.publish(t, prepared)
	c.requirePublishedCommit(t, root, candidate)
	c.requireTreeContent(t, root, candidate.CommitOID, map[string]string{
		"tracked.txt": "checkpoint edit\n", "staged.txt": "staged\n", "untracked.txt": "untracked\n", "sub/nested.txt": "nested\n",
	})
	if after := c.observe(t, root); after != before {
		t.Fatalf("checkpoint changed ordinary state: %+v, want %+v", after, before)
	}
	if linked {
		c.lineage.gitIn(t, c.lineage.main, "worktree", "remove", "--force", root)
	}
	removed, err := c.lineage.git.RemoveCheckpoint(ctx, prepared.request.RootID, candidate)
	if err != nil || !removed {
		t.Fatalf("remove published checkpoint = %v, %v; want removed", removed, err)
	}
	c.requireAbsentRef(t, c.lineage.main, candidate.SnapshotRef)
	if removed, err = c.lineage.git.RemoveCheckpoint(ctx, prepared.request.RootID, candidate); err != nil || removed {
		t.Fatalf("remove absent checkpoint = %v, %v; want verified nothing to remove", removed, err)
	}
}

// requireOrdinaryTarget compares the resolved target with Git's own top level
// and per-worktree admin directory, never an assumed .git path.
func (c checkpointGit) requireOrdinaryTarget(t *testing.T, root string, target workspace.CheckpointTarget) {
	t.Helper()
	admin := c.lineage.gitIn(t, root, "rev-parse", "--path-format=absolute", "--git-dir")
	common := c.lineage.gitIn(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	source := target.Source
	if target.CheckoutRoot != root || source.Root != root || target.AdminDir != admin || source.GitDir != admin || source.CommonDir != common || source.RelativeCwd != "sub" {
		t.Fatalf("ordinary target = %+v, want root %s, admin %s, common %s and sub", target, root, admin, common)
	}
	if _, pinned := source.CommonDirIdentity(); !pinned {
		t.Fatal("ordinary target common directory is not pinned")
	}
}

// A managed checkout keeps its original source base as provenance while the
// checkpoint parent is the checkout's later observed HEAD. The tree is the
// full current content, cumulative from that base, of the mapped subdirectory
// workspace; the child checkout's HEAD, detachment and index never change.
func TestNativeCheckpointManagedKeepsBaseProvenanceAndParentsObservedHEAD(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			c := newCheckpointGit(t, format)
			base := c.commitSubdirectory(t)
			resource, registration := c.lineage.provisionedChild(t, filepath.Join(c.lineage.main, "sub"))
			checkout, err := workspacescore.RegisteredCheckout(c.lineage.scope.ctx, resource)
			if err != nil {
				t.Fatal(err)
			}
			writeLineageFile(t, filepath.Join(checkout.Workdir, "committed.txt"), "child commit\n")
			c.lineage.gitIn(t, checkout.CheckoutPath, "add", "sub/committed.txt")
			childHEAD := c.lineage.commit(t, checkout.CheckoutPath, "child")
			c.dirty(t, checkout.CheckoutPath)
			before := c.observe(t, checkout.CheckoutPath)
			request, digest := c.request(t, &workspace.CheckpointSelection{Kind: workspace.CheckpointManaged}, registration.WorkspaceID)
			candidate, err := c.lineage.git.PrepareManagedCheckpoint(c.lineage.scope.ctx, request, digest, checkout, c.scratch)
			if err != nil {
				t.Fatalf("prepare managed checkpoint: %v", err)
			}
			c.requireScratchEmpty(t)
			c.requireManagedCandidate(t, candidate, checkout, base, childHEAD)
			c.publish(t, preparedCheckpoint{candidate: candidate, request: request, requestSHA256: digest})
			c.requirePublishedCommit(t, c.lineage.main, candidate)
			c.requireTreeContent(t, c.lineage.main, candidate.CommitOID, map[string]string{
				"sub/nested.txt": "nested\n", "sub/committed.txt": "child commit\n", "untracked.txt": "untracked\n", "tracked.txt": "checkpoint edit\n",
			})
			changed := strings.Split(c.lineage.gitIn(t, c.lineage.main, "diff", "--name-only", base, candidate.CommitOID), "\n")
			if want := []string{"staged.txt", "sub/committed.txt", "tracked.txt", "untracked.txt"}; !slices.Equal(changed, want) {
				t.Fatalf("cumulative checkpoint changes from base = %q, want %q", changed, want)
			}
			if after := c.observe(t, checkout.CheckoutPath); after != before || after.attached {
				t.Fatalf("checkpoint changed managed checkout state: %+v, want detached %+v", after, before)
			}
		})
	}
}

func (c checkpointGit) requireManagedCandidate(t *testing.T, candidate workspace.CheckpointCandidate, checkout workspace.Checkout, base, childHEAD string) {
	t.Helper()
	target := candidate.Target
	if target.Source != checkout.Source || target.CheckoutRoot != checkout.CheckoutPath || target.AdminDir != checkout.AdminDir {
		t.Fatalf("managed target = %+v, want the registered checkout %+v", target, checkout)
	}
	if target.Source.BaseOID != base || candidate.ObservedHEAD != childHEAD || childHEAD == base {
		t.Fatalf("managed candidate base/HEAD = %s/%s, want original base %s and later child HEAD %s", target.Source.BaseOID, candidate.ObservedHEAD, base, childHEAD)
	}
	if c.lineage.gitSucceeds(t, c.lineage.main, "cat-file", "-e", candidate.CommitOID) {
		t.Fatal("managed preparation wrote the checkpoint commit object")
	}
	c.requireAbsentRef(t, c.lineage.main, candidate.SnapshotRef)
}
