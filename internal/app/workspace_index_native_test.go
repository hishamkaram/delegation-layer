package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// nativeIndexProbe captures one managed checkout through the production
// private-index algorithm and checks its semantic index digest against the
// complete native `ls-files --stage -v -z` inventory framed with the native
// intent-to-add-invisible entry list.
type nativeIndexProbe struct {
	lineage  nativeLineage
	checkout workspace.Checkout
	index    string
}

func newNativeIndexProbe(t *testing.T) nativeIndexProbe {
	t.Helper()
	return newNativeIndexProbeFormat(t, "sha1")
}

func newNativeIndexProbeFormat(t *testing.T, objectFormat string) nativeIndexProbe {
	t.Helper()
	lineage := newNativeLineageFormat(t, objectFormat)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	checkout, err := workspacescore.RegisteredCheckout(lineage.scope.ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	return nativeIndexProbe{lineage: lineage, checkout: checkout, index: filepath.Join(registration.AdminDir, "index")}
}

func (p nativeIndexProbe) git(t *testing.T, args ...string) string {
	t.Helper()
	return p.lineage.gitIn(t, p.checkout.CheckoutPath, args...)
}

// capture returns the candidate and proves the capture left the real index
// bytes and HEAD untouched and digested exactly the native inventory.
func (p nativeIndexProbe) capture(t *testing.T) workspace.CaptureCandidate {
	t.Helper()
	indexBefore, head := p.indexBytes(t), p.git(t, "rev-parse", "HEAD")
	want := p.nativeIdentity(t)
	candidate, err := p.lineage.git.BuildCapture(p.lineage.scope.ctx, p.checkout, supervisorfixture.CanonicalTempDir(t))
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !bytes.Equal(indexBefore, p.indexBytes(t)) || p.git(t, "rev-parse", "HEAD") != head {
		t.Fatal("capture changed the real index or HEAD")
	}
	if candidate.IndexSHA256 != want {
		t.Fatalf("index digest = %s, want the framed native inventory digest %s", candidate.IndexSHA256, want)
	}
	return candidate
}

func (p nativeIndexProbe) nativeIdentity(t *testing.T) string {
	t.Helper()
	inventory := task.ComputeSHA256([]byte(p.git(t, "ls-files", "--stage", "-v", "-z")))
	emptyTree := p.git(t, "hash-object", "-t", "tree", "--stdin")
	visible := task.ComputeSHA256([]byte(p.git(t, "--no-replace-objects", "diff-index", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--ita-invisible-in-index", emptyTree)))
	return task.ComputeSHA256([]byte(inventory + "\n" + visible + "\n"))
}

func (p nativeIndexProbe) indexBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(p.index)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNativeIndexFingerprintCoversFlagsButNotStatCache(t *testing.T) {
	probe := newNativeIndexProbe(t)
	base := probe.capture(t)
	tracked := filepath.Join(probe.checkout.CheckoutPath, "tracked.txt")
	refreshed := probe.indexBytes(t)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(tracked, future, future); err != nil {
		t.Fatal(err)
	}
	probe.git(t, "update-index", "--refresh")
	if bytes.Equal(refreshed, probe.indexBytes(t)) {
		t.Fatal("fixture did not refresh the real index stat cache")
	}
	if refresh := probe.capture(t); refresh.IndexSHA256 != base.IndexSHA256 {
		t.Fatalf("stat-cache refresh changed the semantic index digest: %s != %s", refresh.IndexSHA256, base.IndexSHA256)
	}
	digests := map[string]string{}
	for _, flag := range []string{"assume-unchanged", "skip-worktree"} {
		probe.git(t, "update-index", "--"+flag, "tracked.txt")
		flagged := probe.capture(t)
		if flagged.IndexSHA256 == base.IndexSHA256 || flagged.TreeOID != base.TreeOID || flagged.ObservedHEAD != base.ObservedHEAD {
			t.Fatalf("%s-only edit: digest %s, tree %s; base digest %s, tree %s", flag, flagged.IndexSHA256, flagged.TreeOID, base.IndexSHA256, base.TreeOID)
		}
		digests[flag] = flagged.IndexSHA256
		probe.git(t, "update-index", "--no-"+flag, "tracked.txt")
		if restored := probe.capture(t); restored.IndexSHA256 != base.IndexSHA256 {
			t.Fatalf("clearing %s did not restore the canonical digest", flag)
		}
	}
	if digests["assume-unchanged"] == digests["skip-worktree"] {
		t.Fatal("assume-unchanged and skip-worktree produced the same digest")
	}
}

func TestNativeIndexFingerprintCoversConflictStagesAndOddPaths(t *testing.T) {
	probe := newNativeIndexProbe(t)
	odd := "odd name\twith tab\nand newline ü"
	writeLineageFile(t, filepath.Join(probe.checkout.CheckoutPath, odd), "odd\n")
	probe.git(t, "add", "--", odd)
	staged := probe.capture(t)
	blob := probe.git(t, "rev-parse", ":"+odd)
	zero := strings.Repeat("0", len(blob))
	records := "0 " + zero + "\t" + odd + "\x00"
	for stage := 1; stage <= 3; stage++ {
		records += fmt.Sprintf("100644 %s %d\t%s\x00", blob, stage, odd)
	}
	probe.lineage.gitInput(t, probe.checkout.CheckoutPath, records, "update-index", "-z", "--index-info")
	if inventory := probe.git(t, "ls-files", "--stage", "-v", "-z"); strings.Count(inventory, "M 100644 "+blob) != 3 {
		t.Fatalf("fixture did not create three unmerged stages:\n%q", inventory)
	}
	conflicted := probe.capture(t)
	if conflicted.IndexSHA256 == staged.IndexSHA256 {
		t.Fatal("conflict stages did not change the semantic index digest")
	}
}

func TestNativeIndexFingerprintCoversLargeInventory(t *testing.T) {
	probe := newNativeIndexProbe(t)
	bulk := filepath.Join(probe.checkout.CheckoutPath, "zz-bulk")
	if err := os.Mkdir(bulk, 0o700); err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("p", 96)
	last := ""
	for index := range 9000 {
		last = fmt.Sprintf("%s-%05d", padding, index)
		writeLineageFile(t, filepath.Join(bulk, last), "bulk\n")
	}
	probe.git(t, "add", "-A")
	if inventory := probe.git(t, "ls-files", "--stage", "-v", "-z"); len(inventory) <= 1<<20 {
		t.Fatalf("fixture inventory is %d bytes, want more than 1 MiB", len(inventory))
	}
	large := probe.capture(t)
	probe.git(t, "update-index", "--assume-unchanged", "zz-bulk/"+last)
	if flagged := probe.capture(t); flagged.IndexSHA256 == large.IndexSHA256 {
		t.Fatal("a flag on the final inventory record did not change the digest")
	}
}

// An intent-to-add entry and a staged empty file print the same ls-files
// record; the index identity still tells them apart while tree and HEAD stay
// the same.
func TestNativeIndexFingerprintDistinguishesIntentToAddFromStagedEmpty(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) { assertIntentToAddDistinguished(t, newNativeIndexProbeFormat(t, objectFormat)) })
	}
}

func assertIntentToAddDistinguished(t *testing.T, probe nativeIndexProbe) {
	t.Helper()
	writeLineageFile(t, filepath.Join(probe.checkout.CheckoutPath, "empty-new.txt"), "")
	probe.git(t, "add", "-N", "empty-new.txt")
	intent := probe.capture(t)
	inventory := probe.git(t, "ls-files", "--stage", "-v", "-z")
	probe.git(t, "add", "empty-new.txt")
	if staged := probe.git(t, "ls-files", "--stage", "-v", "-z"); staged != inventory {
		t.Fatalf("fixture premise changed: ls-files now distinguishes intent-to-add:\n%q\n%q", inventory, staged)
	}
	staged := probe.capture(t)
	if staged.IndexSHA256 == intent.IndexSHA256 || staged.TreeOID != intent.TreeOID || staged.ObservedHEAD != intent.ObservedHEAD {
		t.Fatalf("intent-to-add to staged empty: digest %s -> %s, tree %s -> %s", intent.IndexSHA256, staged.IndexSHA256, intent.TreeOID, staged.TreeOID)
	}
}

// Moving HEAD without changing index entries keeps the index identity.
func TestNativeIndexFingerprintIgnoresHEADOnlyMove(t *testing.T) {
	probe := newNativeIndexProbe(t)
	before := probe.capture(t)
	probe.lineage.commit(t, probe.checkout.CheckoutPath, "HEAD-only checkpoint")
	after := probe.capture(t)
	if after.ObservedHEAD == before.ObservedHEAD || after.IndexSHA256 != before.IndexSHA256 {
		t.Fatalf("HEAD-only move: HEAD %s -> %s, digest %s -> %s", before.ObservedHEAD, after.ObservedHEAD, before.IndexSHA256, after.IndexSHA256)
	}
}

// One owned live-index transaction lands after the capture froze its index
// image and before the first index observation. Both observations still
// describe the frozen image, and the live change stays outside the capture.
func TestNativeIndexObservationsReadOneFrozenImage(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		for _, layout := range []string{"regular", "split"} {
			t.Run(objectFormat+"-"+layout, func(t *testing.T) { assertFrozenIndexImage(t, objectFormat, layout == "split") })
		}
	}
}

func assertFrozenIndexImage(t *testing.T, objectFormat string, split bool) {
	t.Helper()
	probe := newNativeIndexProbeFormat(t, objectFormat)
	if split {
		probe.git(t, "update-index", "--split-index")
		if shared, err := filepath.Glob(filepath.Join(filepath.Dir(probe.index), "sharedindex.*")); err != nil || len(shared) == 0 {
			t.Fatalf("fixture has no shared index: %v, %v", shared, err)
		}
	}
	frozen := probe.nativeIdentity(t)
	blob := probe.git(t, "rev-parse", ":tracked.txt")
	mutated := false
	launch := func(command *exec.Cmd) error {
		if !mutated && slices.Contains(command.Args, "ls-files") {
			mutated = true
			probe.git(t, "update-index", "--add", "--cacheinfo", "100644,"+blob+",late.txt")
		}
		return probe.lineage.launch(command)
	}
	git := workspace.Git{Runner: nativeWorkspaceGitRunner{launch: launch}}
	candidate, err := git.BuildCapture(probe.lineage.scope.ctx, probe.checkout, supervisorfixture.CanonicalTempDir(t))
	if err != nil || !mutated {
		t.Fatalf("capture with a concurrent live-index change: mutated=%v, %v", mutated, err)
	}
	if candidate.IndexSHA256 != frozen || candidate.IndexSHA256 == probe.nativeIdentity(t) {
		t.Fatalf("index digest %s, want the frozen image %s rather than the changed live index", candidate.IndexSHA256, frozen)
	}
	if live := probe.git(t, "ls-files", "--", "late.txt"); live != "late.txt" {
		t.Fatalf("capture changed the live index transaction: %q", live)
	}
}

// Native Git reads a missing index as empty; capture observes the same and
// does not create a real index.
func TestNativeIndexObservationTreatsMissingIndexAsEmpty(t *testing.T) {
	probe := newNativeIndexProbe(t)
	if err := os.Remove(probe.index); err != nil {
		t.Fatal(err)
	}
	empty := probe.nativeIdentity(t)
	candidate, err := probe.lineage.git.BuildCapture(probe.lineage.scope.ctx, probe.checkout, supervisorfixture.CanonicalTempDir(t))
	if err != nil || candidate.IndexSHA256 != empty {
		t.Fatalf("capture without a real index = %s, %v; want the native empty-index identity %s", candidate.IndexSHA256, err, empty)
	}
	if _, err = os.Stat(probe.index); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture created a real index: %v", err)
	}
}

// A replace ref on the empty tree may change Git's own visible-entry listing,
// and how a replaced empty tree reads varies by Git version. The project
// contract is that capture ignores replacements: it succeeds and keeps the
// index identity, HEAD and real index bytes.
func TestNativeIndexIdentityIgnoresEmptyTreeReplacement(t *testing.T) {
	probe := newNativeIndexProbe(t)
	before := probe.capture(t)
	assertReplaceRefsEnabled(t, probe)
	emptyTree := probe.git(t, "hash-object", "-w", "-t", "tree", "--stdin")
	headTree := probe.git(t, "rev-parse", "HEAD^{tree}")
	probe.git(t, "replace", "-f", emptyTree, headTree)
	if raw := probe.git(t, "--no-replace-objects", "for-each-ref", "--format=%(objectname)", "refs/replace/"+emptyTree); raw != headTree {
		t.Fatalf("fixture replace ref = %q, want the HEAD tree %s", raw, headTree)
	}
	if after := probe.capture(t); after.IndexSHA256 != before.IndexSHA256 || after.ObservedHEAD != before.ObservedHEAD {
		t.Fatalf("empty-tree replacement changed the capture: digest %s -> %s, HEAD %s -> %s", before.IndexSHA256, after.IndexSHA256, before.ObservedHEAD, after.ObservedHEAD)
	}
}

// Replacement must be active for the fixture to test anything. The check
// reads only the named variables and Git's boolean setting, never objects.
func assertReplaceRefsEnabled(t *testing.T, probe nativeIndexProbe) {
	t.Helper()
	for _, name := range []string{"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE"} {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("fixture environment sets %s; replace refs must use Git's default", name)
		}
	}
	if enabled := probe.git(t, "config", "--type=bool", "--default=true", "--get", "core.useReplaceRefs"); enabled != "true" {
		t.Fatalf("core.useReplaceRefs = %q, want replace refs enabled", enabled)
	}
}
