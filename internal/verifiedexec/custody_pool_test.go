package verifiedexec

// Pool tests: the shared matrix against the pool, verified publication, layout
// refusals and context behavior. Authority to remove views belongs to the
// caller of ReleaseOwner and is outside these tests.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func TestPoolMutationMatrix(t *testing.T)      { runMatrix(t, poolBinderFactory) }
func TestPoolNodeEntry(t *testing.T)           { runNodeEntry(t, poolBinderFactory) }
func TestPoolInterpreterSnapshot(t *testing.T) { runInterpreterCases(t, poolBinderFactory) }
func TestPoolViewIsNeverTheOriginalPath(t *testing.T) {
	requireViewOutsideOriginal(t, poolBinderFactory)
}

func TestPoolSharesOneVerifiedImagePerDigest(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "shared")
	rig.binder("a", plainRequest(entry, digest))
	rig.binder("b", plainRequest(entry, digest))
	if got := rig.images(); len(got) != 1 || got[0] != digest {
		t.Fatalf("images %v; want exactly %s", got, digest)
	}
	rig.requireImagesDir(1)
	for _, owner := range []string{"a", "b"} {
		rig.requireViewsLinkImages(owner, entry)
	}
	info, err := os.Lstat(rig.pool.imagePath(digest))
	if err != nil {
		t.Fatal(err)
	}
	if count, countErr := linkCount(info); countErr != nil || count != 3 {
		t.Fatalf("image link count %d (%v); want the image plus two views", count, countErr)
	}
	rig.rec.note("shared-image", "two owners, one image, link count 3, mode %o", info.Mode().Perm())
}

func TestPoolWitnessExistsBeforeAnythingIsPublished(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "witness")
	seen := false
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point != pointBeforeCopy {
			return
		}
		seen = true
		if _, err := os.Lstat(rig.ownerDir("a")); err != nil {
			t.Errorf("witness missing when the first stage is about to be written: %v", err)
		}
		if len(rig.images()) != 0 {
			t.Error("an image existed before the witness")
		}
	}
	rig.binder("a", plainRequest(entry, digest))
	if !seen {
		t.Fatal("the before-copy point never fired; the test is void")
	}
}

func TestPoolCopyHashesBytesActuallyWritten(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "mutating-copy")
	mutated := false
	rig.pool.faults.at = func(point poolPoint, got string) {
		if point != pointBeforeCopy || got != digest {
			return
		}
		// Same inode as the admitted descriptor: the source changes after
		// admission and before the first copied byte.
		mutated = true
		if err := mutations[1].apply(entry, []byte("\x7fCUSTODY-FIXTURE mutated!\n")); err != nil {
			t.Errorf("mutate during copy: %v", err)
		}
	}
	_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
	if !mutated {
		t.Fatal("the before-copy point never fired; the test is void")
	}
	if err == nil || !strings.Contains(err.Error(), "snapshot does not match admission") {
		t.Fatalf("bind error %v; want a snapshot mismatch", err)
	}
	rig.requireImagesDir(0)
	rig.requireViewDirs("a", 0)
}

func TestPoolPrebindChangeRefuses(t *testing.T) {
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			rig := newPoolRig(t)
			entry, digest := plainEntry(t, rig.dir, "prebind")
			mutateAndConfirm(t, m, entry, []byte("\x7fCUSTODY-FIXTURE changed\n"), digest)
			_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
			if err == nil || !strings.Contains(err.Error(), "does not match admission") {
				t.Fatalf("bind accepted bytes that differ from admission: %v", err)
			}
			rig.requireImagesDir(0)
			if pending, pendingErr := rig.pool.ReleasePending(rig.ns("a")); pendingErr != nil || pending {
				t.Fatalf("a refused pre-bind change left a witness: %v %v", pending, pendingErr)
			}
		})
	}
}

func TestPoolExistingImageIsReverifiedAndNeverOverwritten(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "reverify")
	rig.binder("a", plainRequest(entry, digest))
	image := rig.pool.imagePath(digest)
	copies := 0
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointBeforeCopy {
			copies++
		}
	}
	if err := os.Chmod(image, 0o700); err != nil {
		t.Fatal(err)
	}
	// Same-user tamper is outside the claim; the point is the refusal and that a
	// bad image is never silently replaced.
	if err := mutations[1].apply(image, []byte("\x7fCUSTODY-FIXTURE tampered\n")); err != nil {
		t.Fatal(err)
	}
	tampered, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rig.pool.Bind(context.Background(), rig.ns("b"), plainRequest(entry, digest))
	if err == nil || !strings.Contains(err.Error(), "verify custody image") {
		t.Fatalf("second bind error %v; want a verification refusal", err)
	}
	after, readErr := os.ReadFile(image)
	if readErr != nil || string(after) != string(tampered) {
		t.Fatalf("a refused bind rewrote the image: %v", readErr)
	}
	if copies != 0 {
		t.Fatalf("%d copies: a corrupt image must not be repopulated over", copies)
	}
}

// plantWinner makes a create-once rival appear between our stage and our link.
func plantWinner(rig *poolRig, digest string, content []byte) {
	image := rig.pool.imagePath(digest)
	rig.pool.faults.at = func(point poolPoint, got string) {
		if point == pointAfterStage && got == digest {
			if err := os.WriteFile(image, content, poolImageMode); err != nil {
				rig.t.Errorf("plant winner: %v", err)
			}
		}
	}
}

func TestPoolCreateOnceValidWinnerIsVerifiedAndUsed(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "winner")
	content, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	plantWinner(rig, digest, content)
	rig.binder("a", plainRequest(entry, digest))
	files := rig.requireViewsLinkImages("a", entry)
	rig.requireImagesDir(1)
	rig.rec.note("create-once", "lost the race, verified the winner, linked %d view file(s) to it", len(files))
}

func TestPoolCreateOnceConflictingWinnerIsRefused(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "conflict")
	rival := []byte("not the admitted bytes")
	plantWinner(rig, digest, rival)
	_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
	if !errors.Is(err, errPoolImageConflict) {
		t.Fatalf("bind error %v, want %v", err, errPoolImageConflict)
	}
	after, readErr := os.ReadFile(rig.pool.imagePath(digest))
	if readErr != nil || string(after) != string(rival) {
		t.Fatalf("create-once publication replaced the existing image: %v", readErr)
	}
	rig.requireImagesDir(1)
	rig.requireViewDirs("a", 0)
}

func TestPoolCrossDeviceLinkIsRefusedNotCopied(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "exdev")
	rig.pool.faults.link = func(string, string) error { return &os.LinkError{Op: "link", Err: unix.EXDEV} }
	_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
	if !errors.Is(err, errPoolCrossDevice) || !errors.Is(err, unix.EXDEV) {
		t.Fatalf("injected EXDEV gave %v; want the explicit cross-device refusal", err)
	}
	rig.requireViewDirs("a", 0)
	rig.requireImagesDir(0)

	other := otherFilesystemDir(t, rig.dir)
	if other == "" {
		rig.rec.note("real-exdev", "not run: no second writable filesystem; set %s", proofOtherFSEnv)
		return
	}
	// A real kernel EXDEV from linking a pool image into another filesystem is
	// classified by the production mapping, never by a copy fallback.
	rig.pool.faults.link = nil
	rig.binder("real", plainRequest(entry, digest))
	linkErr := os.Link(rig.pool.imagePath(digest), filepath.Join(other, "view-entry"))
	if !errors.Is(linkFailure(linkErr), errPoolCrossDevice) {
		t.Fatalf("real cross-filesystem link gave %v; want %v", linkErr, errPoolCrossDevice)
	}
	rig.rec.note("real-exdev", "kernel refused with %v; classified as %v", linkErr, errPoolCrossDevice)
}

// otherFilesystemDir returns a writable directory on a different device than
// dir, or "" when this host offers none.
func otherFilesystemDir(t *testing.T, dir string) string {
	t.Helper()
	dirDevice := deviceOf(t, statOf(t, dir))
	for _, candidate := range []string{os.Getenv(proofOtherFSEnv), "/dev/shm"} {
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || deviceOf(t, info) == dirDevice {
			continue
		}
		made, mkErr := os.MkdirTemp(candidate, "custody-exdev-")
		if mkErr != nil {
			continue
		}
		t.Cleanup(func() {
			if removeErr := os.RemoveAll(made); removeErr != nil {
				t.Errorf("remove: %v", removeErr)
			}
		})
		return made
	}
	return ""
}

func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// rootCase builds a refused pool root below base. want is the sentinel the pool
// reports for it, from Prepare and from a guarded Bind alike.
type rootCase struct {
	name  string
	setup func(t *testing.T, base string) string
	want  error
}

func (c rootCase) refused(err error) bool { return err != nil && errors.Is(err, c.want) }

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func symlinkTo(t *testing.T, base string) (alias string) {
	t.Helper()
	real := filepath.Join(base, "real")
	mkdirMode(t, real, 0o700)
	alias = filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	return alias
}

func unsafeRootCases() []rootCase {
	return []rootCase{
		{name: "group-readable root", want: errPoolRootNotPrivate, setup: func(t *testing.T, base string) string {
			root := filepath.Join(base, "open")
			mkdirMode(t, root, 0o755)
			return root
		}},
		{name: "root is a file", want: errPoolRootNotPrivate, setup: func(t *testing.T, base string) string {
			root := filepath.Join(base, "file")
			if err := os.WriteFile(root, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return root
		}},
		{name: "root is a symlink", want: errPoolRootNotCanon, setup: func(t *testing.T, base string) string {
			return symlinkTo(t, base)
		}},
		{name: "root below a symlinked parent", want: errPoolRootNotCanon, setup: func(t *testing.T, base string) string {
			return filepath.Join(symlinkTo(t, base), "pool")
		}},
	}
}

func TestPoolRefusesAnUnsafeRootBeforePublishing(t *testing.T) {
	entry, digest := plainEntry(t, scratch(t), "layout")
	for _, tc := range unsafeRootCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.setup(t, scratch(t))
			unbound, err := NewPool(root)
			if err != nil {
				t.Fatal(err)
			}
			if id, prepareErr := unbound.Prepare(context.Background()); !tc.refused(prepareErr) {
				t.Fatalf("Prepare gave %q, %v; want %v", id, prepareErr, tc.want)
			}
			guarded, err := NewPoolWithIdentity(root, strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			_, err = guarded.Bind(context.Background(), mustNamespace(t, "a"), plainRequest(entry, digest))
			if !tc.refused(err) {
				t.Fatalf("bind gave %v; want %v", err, tc.want)
			}
			for _, name := range []string{poolImagesDir, poolViewsDir, poolIdentityName} {
				if _, statErr := os.Lstat(filepath.Join(root, name)); statErr == nil {
					t.Fatalf("%s was created below a refused root", name)
				}
			}
		})
	}
}

func TestNewPoolRefusesRootsThatAreNotAbsoluteAndClean(t *testing.T) {
	for _, bad := range []string{"relative/pool", "/tmp/../pool", ""} {
		if _, err := NewPool(bad); !errors.Is(err, errPoolRootInvalid) {
			t.Fatalf("NewPool(%q) gave %v; want %v", bad, err, errPoolRootInvalid)
		}
	}
}

func TestPoolRefusesANoexecFilesystemExplicitly(t *testing.T) {
	rig := newPoolRig(t)
	for _, candidate := range []string{"/dev/shm", os.Getenv(proofOtherFSEnv)} {
		if candidate == "" {
			continue
		}
		noExec, err := filesystemForbidsExec(candidate)
		if err != nil || !noExec {
			continue
		}
		root, mkErr := os.MkdirTemp(candidate, "custody-noexec-")
		if mkErr != nil {
			continue
		}
		t.Cleanup(func() {
			if removeErr := os.RemoveAll(root); removeErr != nil {
				t.Errorf("remove: %v", removeErr)
			}
		})
		requireCleanNoexecRefusal(t, root)
		rig.rec.note("noexec", "%s refused with %v", candidate, ErrPoolNoExec)
		return
	}
	rig.rec.note("noexec", "not run: this host offers no writable noexec mount (checked /dev/shm and %s)", proofOtherFSEnv)
}

// requireCleanNoexecRefusal prepares a pool on a noexec root and requires a clean
// refusal: every leaf of the error tree is the noexec cause, which is what a
// caller classifies with the canonical walker. The canonical lock opener may
// already have created the empty gate; no marker, image or view is ever
// published below a noexec root.
func requireCleanNoexecRefusal(t *testing.T, root string) {
	t.Helper()
	pool, err := NewPool(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := pool.Prepare(context.Background())
	if !errors.Is(err, ErrPoolNoExec) || id != "" {
		t.Fatalf("Prepare below a noexec mount gave %q, %v; want %v", id, err, ErrPoolNoExec)
	}
	if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrPoolNoExec) }) {
		t.Fatalf("the noexec refusal %v has leaves other than %v", err, ErrPoolNoExec)
	}
	for _, name := range []string{poolIdentityName, poolImagesDir, poolViewsDir} {
		if _, statErr := os.Lstat(filepath.Join(root, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("a refused noexec Prepare published %s: %v", name, statErr)
		}
	}
}

func mustNamespace(t *testing.T, name string) Namespace {
	t.Helper()
	ns, err := NewNamespace(name)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func TestNamespaceValidation(t *testing.T) {
	for _, valid := range []string{"a", "root0-task-3f9a", "inspection_7.v2", "0123456789abcdef", strings.Repeat("a", maxNamespaceBytes)} {
		if _, err := NewNamespace(valid); err != nil {
			t.Fatalf("%q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", ".", "..", ".hidden", "a/b", "a\\b", "a..b", "a b", "a\x00b", "é", "A", "Inspection_7.v2", "aB", strings.Repeat("a", maxNamespaceBytes+1)} {
		if _, err := NewNamespace(invalid); !errors.Is(err, errNamespaceInvalid) {
			t.Fatalf("%q gave %v; want %v", invalid, err, errNamespaceInvalid)
		}
	}
	pool, err := NewPool(filepath.Join(scratch(t), "pool"))
	if err != nil {
		t.Fatal(err)
	}
	// The namespace is judged before the identity, so even an unbound pool
	// reports the invalid namespace.
	if _, err = pool.Bind(context.Background(), Namespace{}, CommandRequest{}); !errors.Is(err, errNamespaceInvalid) {
		t.Fatalf("the zero namespace reached Bind: %v", err)
	}
	if err = pool.ReleaseOwner(context.Background(), Namespace{}); !errors.Is(err, errNamespaceInvalid) {
		t.Fatalf("the zero namespace reached ReleaseOwner: %v", err)
	}
	if _, err = pool.ReleasePending(Namespace{}); !errors.Is(err, errNamespaceInvalid) {
		t.Fatalf("the zero namespace reached ReleasePending: %v", err)
	}
}

func TestPoolCloseRetainsTheViewAndPrivateCloseRemovesIt(t *testing.T) {
	dir := scratch(t)
	rec := newRecorder(t)
	rig := rigIn(t, dir)
	entry, digest := plainEntry(t, dir, "close")
	pooled, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
	if err != nil {
		t.Fatal(err)
	}
	if err = pooled.Close(); err != nil {
		t.Fatalf("pooled Close: %v", err)
	}
	if _, err = os.Lstat(pooled.temporaryDir); err != nil {
		t.Fatalf("pooled Close removed a lifetime-owned view: %v", err)
	}
	private, err := NewCommand(entry, digest, []string{"TMPDIR=" + dir})
	if err != nil {
		t.Fatal(err)
	}
	if err = private.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(private.temporaryDir); !os.IsNotExist(err) {
		t.Fatalf("private Close left its view: %v", err)
	}
	rec.note("close", "pooled Close kept %s; private Close removed its own", pooled.temporaryDir)
}

func TestPoolBindHonorsContextAndGateWaits(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "context")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rig.pool.Bind(cancelled, rig.ns("a"), plainRequest(entry, digest)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context gave %v", err)
	}
	rig.requireNothingBound()

	rig.binder("first", plainRequest(entry, digest))
	other, otherDigest := plainEntry(t, rig.dir, "context-other")
	holder, err := taskdir.OpenLockFileContext(context.Background(), rig.pool.gatePath(), taskdir.LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	if err = holder.LockEXNonblocking(); err != nil {
		t.Fatal(err)
	}
	bounded, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	started := time.Now()
	_, err = rig.pool.Bind(bounded, rig.ns("second"), plainRequest(other, otherDigest))
	waited := time.Since(started)
	if unlockErr := errors.Join(holder.Unlock(), holder.Close()); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) || waited > 10*time.Second {
		t.Fatalf("a bind waiting on a held gate gave %v after %s; want the context deadline", err, waited)
	}
	rig.requireViewDirs("second", 0)
	rig.rec.note("gate-wait", "bind returned %v after %s behind an exclusive holder; no partial view remained", err, waited.Round(time.Millisecond))
}

func TestPoolNativeFactsOfASharedImage(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := selfNative(t, filepath.Join(rig.dir, "native"))
	command := rig.binder("a", CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg}})
	report, err := runToCompletion(command)
	if err != nil {
		t.Fatal(err)
	}
	if report.Marker != digest {
		t.Fatalf("the view ran %s, want the admitted %s", report.Marker, digest)
	}
	image := rig.pool.imagePath(digest)
	facts, err := filesystemFacts(rig.pool.root)
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := listXattrs(image)
	if err != nil {
		attributes = "unavailable: " + err.Error()
	}
	rig.rec.note("pool-filesystem", "%s", facts)
	rig.rec.note("image-xattrs", "%q (a hard link shares the inode, so the view carries the same attributes)", attributes)
	rig.rec.note("image-code-signature", "%s", codeSignatureFacts(image))
	rig.rec.note("view-code-signature", "%s", codeSignatureFacts(command.Cmd.Path))
}

func TestPoolRefusesANilContextBeforeAnyMutation(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "nil-context")
	var nilContext context.Context
	if _, err := rig.pool.Bind(nilContext, rig.ns("a"), plainRequest(entry, digest)); !errors.Is(err, errNilContext) {
		t.Fatalf("Bind with a nil context gave %v; want %v", err, errNilContext)
	}
	rig.requireNothingBound()
	rig.binder("a", plainRequest(entry, digest))
	if err := rig.pool.ReleaseOwner(nilContext, rig.ns("a")); !errors.Is(err, errNilContext) {
		t.Fatalf("ReleaseOwner with a nil context gave %v; want %v", err, errNilContext)
	}
	if !rig.pending("a") {
		t.Fatal("a refused ReleaseOwner removed the witness")
	}
	rig.requireViewsLinkImages("a", entry)
	if len(rig.logged()) != 0 {
		t.Fatalf("a refused call ran maintenance: %q", rig.logged())
	}
}

// decoyCopy writes a regular file with the given bytes next to the pool, to be
// linked by a faulted link in place of the verified image.
func decoyCopy(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o500); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPoolRefusesAViewEntryThatIsNotTheVerifiedImage(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "samefile")
	rig.binder("a", plainRequest(entry, digest))
	content, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	// Identical bytes, different inode: only the same-inode check can tell.
	decoy := decoyCopy(t, rig.dir, "decoy-same-bytes", content)
	rig.pool.faults.link = func(_, newname string) error { return os.Link(decoy, newname) }
	_, err = rig.pool.Bind(context.Background(), rig.ns("b"), plainRequest(entry, digest))
	if err == nil || !strings.Contains(err.Error(), "not the verified image") {
		t.Fatalf("a view entry that is not the verified inode gave %v", err)
	}
	rig.requireViewDirs("b", 0)
}

func TestPoolRefusesAPublishedImageThatDoesNotHashToItsName(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "bad-publish")
	wrong := decoyCopy(t, rig.dir, "decoy-wrong-bytes", []byte("not the admitted bytes"))
	rig.pool.faults.link = func(oldname, newname string) error {
		if filepath.Dir(newname) == rig.pool.imagesPath() {
			return os.Link(wrong, newname)
		}
		return os.Link(oldname, newname)
	}
	_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
	if err == nil || !strings.Contains(err.Error(), "verify custody image") {
		t.Fatalf("an image whose bytes do not hash to its name gave %v", err)
	}
	rig.requireViewDirs("a", 0)
	rig.pool.faults.link = nil
	if _, err = rig.pool.Bind(context.Background(), rig.ns("b"), plainRequest(entry, digest)); err == nil {
		t.Fatal("a later bind trusted the corrupt published image")
	}
}

// TestEnsuredDirectoryRepairsItsParentBarrierOnRetry: a namespace directory
// that already exists may have been created by an attempt whose parent
// durability barrier then failed, so every later ensure must repeat the
// barrier before the directory is used for publication.
func TestEnsuredDirectoryRepairsItsParentBarrierOnRetry(t *testing.T) {
	rig := newPoolRig(t)
	seed, seedDigest := plainEntry(t, rig.dir, "barrier-seed")
	rig.binder("seed", plainRequest(seed, seedDigest))
	entry, digest := plainEntry(t, rig.dir, "barrier-retry")
	views := rig.pool.viewsPath()
	// Write and search but no read: Mkdir succeeds, opening the directory for
	// the durability barrier fails.
	if err := os.Chmod(views, 0o300); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if err := os.Chmod(views, 0o700); err != nil {
			t.Errorf("restore views mode: %v", err)
		}
	}
	defer restore()
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := rig.pool.Bind(context.Background(), rig.ns("fresh"), plainRequest(entry, digest)); err == nil {
			t.Fatalf("attempt %d: bind published into a namespace whose parent barrier cannot succeed", attempt)
		}
	}
	restore()
	if rig.imageExists(digest) {
		t.Fatal("an image was published while the namespace barrier was failing")
	}
	rig.binder("fresh", plainRequest(entry, digest))
	rig.requireViewsLinkImages("fresh", entry)
}

// TestStageTamperBetweenWriteAndVerifyIsRefusedBeforePublication: the stage is
// written, made read-only and flushed, then re-opened by digest. A change in
// that window must stop the bind before any image is published, rather than
// leaving a corrupt image for a later link-time hash to find.
func TestStageTamperBetweenWriteAndVerifyIsRefusedBeforePublication(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "stage-tamper")
	fired := 0
	rig.pool.faults.at = func(point poolPoint, got string) {
		if point != pointStageWritten || got != digest {
			return
		}
		fired++
		stages, err := filepath.Glob(filepath.Join(rig.pool.imagesPath(), poolStagePrefix+"*"))
		if err != nil || len(stages) != 1 {
			t.Errorf("stages %v (%v); want exactly one", stages, err)
			return
		}
		if err = os.Chmod(stages[0], 0o700); err != nil {
			t.Error(err)
			return
		}
		if err = os.WriteFile(stages[0], []byte("tampered after the flush"), 0o500); err != nil {
			t.Error(err)
		}
	}
	_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
	if fired != 1 {
		t.Fatalf("the stage-written point fired %d times; the test is void", fired)
	}
	if err == nil {
		t.Fatal("a stage changed after its flush was published")
	}
	if rig.imageExists(digest) {
		t.Fatal("a tampered stage was published as the image")
	}
	rig.requireImagesDir(0)
	rig.requireViewDirs("a", 0)
}
