package verifiedexec

// The pool rig: a Pool below a scratch directory, a captured log, and the
// assertions shared by the pool tests.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const poolImageMode = 0o500

type poolRig struct {
	t    *testing.T
	dir  string
	id   string
	pool *Pool
	rec  *recorder

	mu   sync.Mutex
	logs []string
}

func newPoolRig(t *testing.T) *poolRig {
	t.Helper()
	return rigIn(t, scratch(t))
}

func rigIn(t *testing.T, dir string) *poolRig {
	t.Helper()
	pool, id := preparedPool(t, filepath.Join(dir, "pool"))
	rig := &poolRig{t: t, dir: dir, id: id, pool: pool, rec: newRecorder(t)}
	pool.logf = rig.logf
	return rig
}

// preparedPool writes a literal legacy layout. Production has no creation API.
func preparedPool(t *testing.T, root string) (*Pool, string) {
	t.Helper()
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, dir := range []string{root, filepath.Join(root, "images"), filepath.Join(root, "views")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, raw := range map[string]string{
		".identity": `{"version":1,"pool_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		".gate":     "",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := NewPoolWithIdentity(root, id)
	if err != nil {
		t.Fatal(err)
	}
	return pool, id
}

// legacyView names fixture bytes only; it is never an executable Command.
type legacyView struct{ temporaryDir string }

// seedView models the historical disk layout without any production producer.
// It creates hard links to test-owned image bytes and a sealed owner view.
func (r *poolRig) seedView(owner, original, imageID string) legacyView {
	r.t.Helper()
	source, err := os.Lstat(original)
	if err != nil || !source.Mode().IsRegular() {
		r.t.Fatalf("legacy fixture original is not a regular data file: %s: %v", original, err)
	}
	image := r.pool.imagePath(imageID)
	if _, err = os.Lstat(image); errors.Is(err, os.ErrNotExist) {
		if err = os.WriteFile(image, []byte("historical pool fixture data\n"), 0o500); err != nil {
			r.t.Fatal(err)
		}
	} else if err != nil {
		r.t.Fatal(err)
	}
	witness := r.ownerDir(owner)
	if err = os.MkdirAll(witness, 0o700); err != nil {
		r.t.Fatal(err)
	}
	view, err := os.MkdirTemp(witness, "delegation-layer-verified-")
	if err != nil {
		r.t.Fatal(err)
	}
	if err = os.Link(image, filepath.Join(view, "entry")); err != nil {
		r.t.Fatal(err)
	}
	if err = os.Chmod(view, 0o500); err != nil {
		r.t.Fatal(err)
	}
	return legacyView{temporaryDir: view}
}

func (r *poolRig) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (r *poolRig) logged() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

func (r *poolRig) ns(owner string) Namespace {
	r.t.Helper()
	ns, err := NewNamespace(owner)
	if err != nil {
		r.t.Fatal(err)
	}
	return ns
}

// pending is ReleasePending that fails the test on a concrete error.
func (r *poolRig) pending(owner string) bool {
	r.t.Helper()
	pending, err := r.pool.ReleasePending(r.ns(owner))
	if err != nil {
		r.t.Fatalf("ReleasePending %s: %v", owner, err)
	}
	return pending
}

func (r *poolRig) ownerDir(owner string) string { return r.pool.ownerPath(r.ns(owner)) }

// images lists the digest-named images.
func (r *poolRig) images() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.pool.imagesPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if isDigestName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// imagesDirEntries lists every name in the images directory, stages included.
func (r *poolRig) imagesDirEntries() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.pool.imagesPath())
	if err != nil {
		r.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (r *poolRig) imageExists(digest string) bool {
	r.t.Helper()
	return slices.Contains(r.images(), digest)
}

func (r *poolRig) requireImagesDir(want int) {
	r.t.Helper()
	if got := r.imagesDirEntries(); len(got) != want {
		r.t.Fatalf("images directory holds %v; want %d entries", got, want)
	}
}

func (r *poolRig) viewDirs(owner string) []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.ownerDir(owner))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (r *poolRig) requireViewDirs(owner string, want int) {
	r.t.Helper()
	if got := r.viewDirs(owner); len(got) != want {
		r.t.Fatalf("owner %s has views %v; want %d", owner, got, want)
	}
}

// requireViewsLinkImages asserts that every regular file in owner's views is a
// hard link to the pool image named by its own digest, not the original.
func (r *poolRig) requireViewsLinkImages(owner, original string) []string {
	r.t.Helper()
	files, err := viewEntries(r.ownerDir(owner))
	if err != nil {
		r.t.Fatal(err)
	}
	if len(files) == 0 {
		r.t.Fatal("view has no materialized file")
	}
	originalInfo, err := os.Lstat(original)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, file := range files {
		imageInfo, viewInfo := r.imageOf(file)
		if !os.SameFile(imageInfo, viewInfo) {
			r.t.Fatalf("view file %s is not a hard link to its image", file)
		}
		if os.SameFile(originalInfo, viewInfo) {
			r.t.Fatalf("view file %s is the original, not a snapshot", file)
		}
		if imageInfo.Mode().Perm() != poolImageMode {
			r.t.Fatalf("image mode %v, want %o", imageInfo.Mode().Perm(), poolImageMode)
		}
	}
	return files
}

// imageOf finds the historical image by inode metadata without reading its data.
func (r *poolRig) imageOf(file string) (image, view os.FileInfo) {
	r.t.Helper()
	var err error
	view, err = os.Lstat(file)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, name := range r.images() {
		candidate, statErr := os.Lstat(r.pool.imagePath(name))
		if statErr != nil {
			r.t.Fatal(statErr)
		}
		if os.SameFile(candidate, view) {
			return candidate, view
		}
	}
	r.t.Fatalf("view file %s has no linked historical image", file)

	return image, view
}

// viewEntries lists the regular files in views below root (symlinks are the
// mirrored provider installation and are skipped).
func viewEntries(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// plainEntry creates non-program data beside a literal historical image name.
// The image ID is fixture record data, never derived from executable contents.
func plainEntry(t *testing.T, dir, name, imageID string) (path, digest string) {
	t.Helper()
	path = filepath.Join(dir, "fixture-"+name, "entry.data")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original fixture data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, imageID
}
