package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// An ordinary source has no managed Checkout. After validateSourceIdentity,
// its source identity alone fills the content target: every content command
// runs from the source root, both index observations read one frozen image of
// the source Git directory's real index, and that index stays byte-identical.
func TestCaptureContentBuildsFromValidatedOrdinarySource(t *testing.T) {
	fixture := newCheckoutFixture(t)
	stageRecord := "100644 " + testBlobOID + " 0\tordinary-staged\x00"
	runner := captureRunner(fixture, stageRecord)
	git := Git{Runner: runner}
	source, target := validatedOrdinaryTarget(t, git, fixture.root)
	live := filepath.Join(source.GitDir, "index")
	liveImage := []byte("opaque ordinary index image")
	if err := os.WriteFile(live, liveImage, 0o600); err != nil {
		t.Fatal(err)
	}
	observed := recordFrozenIndexReads(runner, liveImage)
	runner.calls = nil
	treeOID, indexDigest, err := git.buildCaptureContent(context.Background(), target, fixture.scratch)
	if err != nil {
		t.Fatal(err)
	}
	if treeOID != testTreeOID {
		t.Fatalf("tree = %s, want %s", treeOID, testTreeOID)
	}
	if want := wantIndexIdentity("H "+stageRecord, testVisibleIndex); indexDigest != want {
		t.Fatalf("index identity = %s, want framed inventory and visible-entry digests %s", indexDigest, want)
	}
	if images := *observed; len(images) != 2 || images[0] != images[1] || filepath.Dir(images[0]) != fixture.scratch {
		t.Fatalf("index observations read %q, want one frozen image in the capture scratch", images)
	}
	assertPrivateIndexBindings(t, runner.calls, fixture.scratch)
	for _, command := range runner.calls {
		if command.Args[2] != source.Root {
			t.Fatalf("content command %v ran from %q, want source root %q", command.Args[3:], command.Args[2], source.Root)
		}
	}
	if data, readErr := os.ReadFile(live); readErr != nil || !bytes.Equal(data, liveImage) {
		t.Fatalf("source real index changed: %q, %v", data, readErr)
	}
	assertCaptureScratchEmpty(t, fixture.scratch)
}

// Every object name the builder accepts is checked in the target's own object
// format, and a refused build still removes its private files.
func TestCaptureContentRefusesObjectsOutsideTargetFormat(t *testing.T) {
	fixture := newCheckoutFixture(t)
	git := Git{Runner: captureRunner(fixture, "")}
	_, target := validatedOrdinaryTarget(t, git, fixture.root)
	target.objectFormat = "sha256"
	treeOID, indexDigest, err := git.buildCaptureContent(context.Background(), target, fixture.scratch)
	if err == nil || treeOID != "" || indexDigest != "" {
		t.Fatalf("content in a foreign object format = %q, %q, %v; want an explicit failure", treeOID, indexDigest, err)
	}
	assertCaptureScratchEmpty(t, fixture.scratch)
}

// recordFrozenIndexReads requires both index observations to read an image
// holding exactly liveImage and records each image path they read.
func recordFrozenIndexReads(runner *recordingGitRunner, liveImage []byte) *[]string {
	inner := runner.handler
	observed := &[]string{}
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"ls-files", "--stage", "-v", "-z"}) || slices.Equal(args, visibleIndexArgs) {
			image := indexOverride(command.Env)
			data, err := os.ReadFile(image)
			if err != nil || !bytes.Equal(data, liveImage) {
				return errors.Join(errors.New("index observation did not read the frozen source index image"), err)
			}
			*observed = append(*observed, image)
		}
		return inner(command)
	}
	return observed
}

// validatedOrdinaryTarget resolves the fixture's main worktree as an ordinary
// source, validates its identity, and fills the content target from it.
func validatedOrdinaryTarget(t *testing.T, git Git, root string) (Source, captureTarget) {
	t.Helper()
	ctx := context.Background()
	source, err := git.ResolveSource(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = git.validateSourceIdentity(ctx, source); err != nil {
		t.Fatal(err)
	}
	if source.Root != root || source.GitDir != filepath.Join(root, ".git") {
		t.Fatalf("ordinary source = %+v, want the fixture main worktree", source)
	}
	return source, captureTarget{root: source.Root, adminDir: source.GitDir, objectFormat: source.ObjectFormat}
}

func assertCaptureScratchEmpty(t *testing.T, scratch string) {
	t.Helper()
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("capture left private files in scratch: %v", entries)
	}
}
