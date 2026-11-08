package remotefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rooted(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

func entryFor(name, content string) Entry {
	sum := sha256.Sum256([]byte(content))
	return Entry{Path: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
}

func write(t *testing.T, root *os.Root, name, content string) {
	t.Helper()
	if err := root.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedTransferResumesAndMaterializesIndependentBytes(t *testing.T) {
	ctx := context.Background()
	root := rooted(t)
	entry := entryFor("src/main.txt", "hello world")
	manifest := Manifest{Entries: []Entry{entry}}
	stage, err := OpenStage(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = stage.WriteChunk(ctx, entry.Path, 0, []byte("hello ")); err != nil {
		t.Fatal(err)
	}
	// Re-open represents a new invocation; offset is derived from synced bytes.
	stage, err = OpenStage(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	offset, err := stage.Offset(ctx, entry.Path)
	if err != nil || offset != 6 {
		t.Fatalf("offset=%d err=%v", offset, err)
	}
	if err = stage.WriteChunk(ctx, entry.Path, offset, []byte("world")); err != nil {
		t.Fatal(err)
	}
	if err = stage.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	destination := rooted(t)
	if err = stage.Materialize(ctx, destination); err != nil {
		t.Fatal(err)
	}
	actual, err := destination.ReadFile(entry.Path)
	if err != nil || string(actual) != "hello world" {
		t.Fatalf("bytes=%q err=%v", actual, err)
	}
	write(t, destination, entry.Path, "changed")
	offset, err = stage.Offset(ctx, entry.Path)
	if err != nil || offset != 11 {
		t.Fatalf("source mutated: offset=%d err=%v", offset, err)
	}
	if err = stage.Materialize(ctx, destination); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrite accepted: %v", err)
	}
}

func TestCorruptIncompleteAndConflictingTransferNeverCommits(t *testing.T) {
	for _, content := range []string{"wrong", "he"} {
		t.Run(content, func(t *testing.T) {
			root := rooted(t)
			entry := entryFor("file", "hello")
			stage, err := OpenStage(root, Manifest{Entries: []Entry{entry}})
			if err != nil {
				t.Fatal(err)
			}
			if err = stage.WriteChunk(context.Background(), "file", 0, []byte(content)); err != nil {
				t.Fatal(err)
			}
			if err = stage.Commit(context.Background()); !errors.Is(err, ErrConflict) {
				t.Fatalf("corruption accepted: %v", err)
			}
			if err = stage.WriteChunk(context.Background(), "file", 0, []byte("hello")); !errors.Is(err, ErrConflict) {
				t.Fatalf("offset conflict accepted: %v", err)
			}
		})
	}
}

func TestCommitSettlesInterruptedLinkPublication(t *testing.T) {
	root := rooted(t)
	entry := entryFor("file", "data")
	stage, err := OpenStage(root, Manifest{Entries: []Entry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err = stage.WriteChunk(context.Background(), "file", 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	part, complete := stageNames(entry)
	if err = root.Link(part, complete); err != nil {
		t.Fatal(err)
	}
	if err = stage.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = root.Lstat(part); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial retained: %v", err)
	}
}

func TestManifestRejectsUnsafePathsAndAmbiguity(t *testing.T) {
	for _, name := range []string{"../x", "/absolute", "a/../x", "a\\b", ".git/config", "a/.GIT/index", ".delegate-control/x", ""} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateManifest(Manifest{Entries: []Entry{entryFor(name, "x")}}); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, names := range [][]string{{"a", "A"}, {"a", "a/b"}, {"Foo/a", "foo/b"}} {
		manifest := Manifest{}
		for _, name := range names {
			manifest.Entries = append(manifest.Entries, entryFor(name, "x"))
		}
		if err := ValidateManifest(manifest); err == nil {
			t.Fatal("collision accepted")
		}
	}
	large := entryFor("large", "")
	large.Size = MaxFileBytes + 1
	if err := ValidateManifest(Manifest{Entries: []Entry{large}}); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestManifestRejectsLinksAndSymlinkParents(t *testing.T) {
	root := rooted(t)
	write(t, root, "file", "data")
	if err := root.Link("file", "hard"); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildManifest(context.Background(), root, []string{"hard"}); err == nil {
		t.Fatal("hardlink accepted")
	}
	if err := root.Mkdir("dir", 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, root, "dir/nested", "data")
	if err := root.Symlink("dir", "alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildManifest(context.Background(), root, []string{"alias/nested"}); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if err := root.Symlink("dir/nested", "link"); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildManifest(context.Background(), root, []string{"link"}); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestCaptureSelectedFilesAndAbsenceWithoutApplyingChanges(t *testing.T) {
	source, destination := rooted(t), rooted(t)
	write(t, source, "result.txt", "answer")
	write(t, source, "unselected", "private")
	provenance := Provenance{RootID: "root", TaskID: "task", OriginRootID: "origin", InputSHA256: strings.Repeat("a", 64)}
	receipt, err := Capture(context.Background(), source, destination, []string{"result.txt", "missing"}, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Files) != 2 || receipt.Files[0].Status != "captured" || receipt.Files[1].Status != "absent" || receipt.Provenance != provenance || receipt.CapturedAt == "" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if _, err = destination.Lstat("unselected"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("undeclared file transferred")
	}
	bytes, err := destination.ReadFile("result.txt")
	if err != nil || string(bytes) != "answer" {
		t.Fatalf("bytes=%q err=%v", bytes, err)
	}
}

func TestExecutableModeNormalizedAndCanceledTransferPreserved(t *testing.T) {
	source := rooted(t)
	write(t, source, "script", "source script")
	if err := source.Chmod("script", 0o777); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildManifest(context.Background(), source, []string{"script"})
	if err != nil {
		t.Fatal(err)
	}
	stageRoot := rooted(t)
	stage, err := OpenStage(stageRoot, manifest)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = stage.WriteChunk(ctx, "script", 0, []byte("source script")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = stage.WriteChunk(context.Background(), "script", 0, []byte("source script")); err != nil {
		t.Fatal(err)
	}
	if err = stage.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	destination := rooted(t)
	if err = stage.Materialize(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	info, err := destination.Stat("script")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode=%v", info.Mode())
	}
	if filepath.IsAbs(manifest.Entries[0].Path) {
		t.Fatal("manifest exposed absolute path")
	}
}

func TestLostChunkAcknowledgmentReplaysOnlyIdenticalBytes(t *testing.T) {
	root := rooted(t)
	entry := entryFor("file", "hello")
	stage, err := OpenStage(root, Manifest{Entries: []Entry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err = stage.WriteChunk(t.Context(), "file", 0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err = stage.WriteChunk(t.Context(), "file", 0, []byte("hello")); err != nil {
		t.Fatalf("identical replay refused: %v", err)
	}
	if err = stage.WriteChunk(t.Context(), "file", 0, []byte("other")); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay accepted: %v", err)
	}
	if err = stage.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = stage.WriteChunk(t.Context(), "file", 0, []byte("hello")); err != nil {
		t.Fatalf("committed replay refused: %v", err)
	}
}
