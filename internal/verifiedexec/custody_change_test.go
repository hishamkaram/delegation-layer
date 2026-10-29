package verifiedexec

// Provenance of "the admitted executable changed": only the two entrypoint
// constructors can report it, only for a change of the request's own path, and
// the original text, errors.Is and errors.As answers are preserved.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func isAdmittedChange(cause error) bool { return errors.Is(cause, ErrAdmittedExecutableChanged) }

// wholeErrorIsAdmittedChange is the consumer's classification: every leaf of the
// whole returned error is the admitted-change marker.
func wholeErrorIsAdmittedChange(err error) bool { return task.AllErrorCauses(err, isAdmittedChange) }

func TestAdmittedOpenErrorClassifiesTheWholeError(t *testing.T) {
	leaf := func() error { return changedExecutable(errors.New("changed")) }
	closeFault := errors.New("close fault")
	admittedLeaf := &executableChangeError{err: errors.New("already"), admitted: true}
	cases := []struct {
		name string
		err  error
		// admitted is whether the classified error reports the admitted change.
		admitted bool
	}{
		{"single leaf", leaf(), true},
		{"wrapped leaf", fmt.Errorf("outer: %w", leaf()), true},
		{"join with a nil close", errors.Join(leaf(), nil), true},
		{"join of two changes", errors.Join(leaf(), leaf()), true},
		{"nested join of changes", errors.Join(errors.Join(leaf()), leaf()), true},
		{"missing path", missingExecutable(&fs.PathError{Op: "lstat", Path: "/x", Err: unix.ENOENT}), true},
		{"join with a close fault", errors.Join(leaf(), closeFault), false},
		{"join with EIO", errors.Join(leaf(), unix.EIO), false},
		{"EACCES", unix.EACCES, false},
		{"only ENOENT is tagged as missing", missingExecutable(unix.EIO), false},
		{"unknown error", closeFault, false},
		{"untagged ENOENT", &fs.PathError{Op: "open", Path: "/interpreter", Err: unix.ENOENT}, false},
		{"already admitted is left as it is", admittedLeaf, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := admittedOpenError(tc.err)
			if reported := errors.Is(got, ErrAdmittedExecutableChanged); reported != tc.admitted || wholeErrorIsAdmittedChange(got) != tc.admitted {
				t.Fatalf("admittedOpenError(%v) reports the admitted change = %v; want %v", tc.err, reported, tc.admitted)
			}
			if got.Error() != tc.err.Error() {
				t.Fatalf("the diagnostic changed from %q to %q", tc.err, got)
			}
		})
	}
	if got := admittedOpenError(nil); got != nil {
		t.Fatalf("admittedOpenError(nil) = %v", got)
	}
}

func TestChangedExecutableKeepsTheOriginalIsAndAsAnswers(t *testing.T) {
	original := &fs.PathError{Op: "lstat", Path: "/opt/tool/bin/tool", Err: unix.ENOENT}
	for name, err := range map[string]error{
		"tagged cause":   missingExecutable(original),
		"admitted whole": admittedOpenError(errors.Join(missingExecutable(original), nil)),
	} {
		var pathErr *fs.PathError
		if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, unix.ENOENT) || !errors.As(err, &pathErr) || pathErr != original {
			t.Fatalf("%s lost an Is or As answer of %v: %v", name, original, err)
		}
		if err.Error() != original.Error() {
			t.Fatalf("%s changed the text %q to %q", name, original, err)
		}
	}
	var errno unix.Errno
	if !errors.As(missingExecutable(unix.ENOENT), &errno) || errno != unix.ENOENT {
		t.Fatal("errors.As lost the bare errno of a tagged open failure")
	}
	// Observed limit: the legacy predicate does not unwrap, so a tagged error is
	// not recognized by os.IsNotExist. Callers use errors.Is.
	newRecorder(t).note("legacy-isnotexist", "os.IsNotExist(tagged ENOENT) = %v; errors.Is(fs.ErrNotExist) = %v",
		os.IsNotExist(missingExecutable(original)), errors.Is(missingExecutable(original), fs.ErrNotExist))
}

// changeCase is one way an installer changes the admitted entrypoint after it
// was admitted, with the text a digest mismatch keeps.
type changeCase struct {
	name     string
	apply    func(t *testing.T, path string)
	missing  bool
	wantText string
}

func changeCases() []changeCase {
	replacement := []byte("\x7fCUSTODY-FIXTURE changed\n")
	cases := []changeCase{{name: "removed", missing: true, apply: func(t *testing.T, path string) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}}}
	for _, m := range mutations {
		cases = append(cases, changeCase{
			name: m.name, wantText: "runtime executable identity does not match admission",
			apply: func(t *testing.T, path string) {
				if err := m.apply(path, replacement); err != nil {
					t.Fatal(err)
				}
			},
		})
	}
	return cases
}

func (c changeCase) requireTagged(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("a changed original was bound")
	}
	if !errors.Is(err, ErrAdmittedExecutableChanged) || !wholeErrorIsAdmittedChange(err) {
		t.Fatalf("%s: %v is not the admitted change as a whole error", c.name, err)
	}
	var pathErr *fs.PathError
	switch {
	case c.missing && (!errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr)):
		t.Fatalf("%s: the missing original lost its Is or As answers: %v", c.name, err)
	case !c.missing && !strings.Contains(err.Error(), c.wantText):
		t.Fatalf("%s: the diagnostic %q lost %q", c.name, err, c.wantText)
	}
}

func TestPrivateBindTagsAChangedOriginalAsAdmittedChange(t *testing.T) {
	for _, tc := range changeCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := scratch(t)
			entry, digest := plainEntry(t, dir, "private-change")
			tc.apply(t, entry)
			command, err := NewCommandFromRequest(CommandRequest{Executable: entry, SHA256: digest, Environment: []string{"TMPDIR=" + dir}})
			if command != nil {
				t.Errorf("a command was bound to a changed original: %v", command)
			}
			tc.requireTagged(t, err)
			requireNoViewLeft(t, dir)
		})
	}
}

func TestGuardedPoolBindTagsAChangedOriginalAsAdmittedChange(t *testing.T) {
	for _, tc := range changeCases() {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPoolRig(t)
			entry, digest := plainEntry(t, rig.dir, "pool-change")
			tc.apply(t, entry)
			_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
			tc.requireTagged(t, err)
			rig.requireNothingBound()
		})
	}
}

// TestDirectInspectionNeverReportsAnAdmittedChange: Open, Fingerprint and
// LocatePath serve every caller, including the interpreter, image and stage
// checks, so none of them says that an admitted entrypoint changed. Text, Is and
// As are exactly the original ones.
func TestDirectInspectionNeverReportsAnAdmittedChange(t *testing.T) {
	dir := scratch(t)
	entry, digest := plainEntry(t, dir, "direct")
	gone := filepath.Join(dir, "gone", "tool")
	if _, err := Open(entry, strings.Repeat("0", 64)); err == nil || err.Error() != "runtime executable identity does not match admission" || errors.Is(err, ErrAdmittedExecutableChanged) {
		t.Fatalf("Open with the wrong digest gave %v; want the original text and no admitted marker", err)
	}
	for name, run := range map[string]func() error{
		"Open":        func() error { _, err := Open(gone, digest); return err },
		"Fingerprint": func() error { _, err := Fingerprint(gone); return err },
		"LocatePath":  func() error { _, err := LocatePath(gone); return err },
	} {
		err := run()
		var pathErr *fs.PathError
		if err == nil || !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) || errors.Is(err, ErrAdmittedExecutableChanged) {
			t.Fatalf("%s of a missing path gave %v; want ENOENT answers and no admitted marker", name, err)
		}
	}
}

// refusalControl runs one fault that is not "the admitted path changed" and
// returns the error the binding gave.
type refusalControl struct {
	name string
	run  func(t *testing.T) error
}

func entryRequest(dir, entry, digest string) CommandRequest {
	return CommandRequest{Executable: entry, SHA256: digest, Environment: []string{"TMPDIR=" + dir}}
}

func refusalControls() []refusalControl {
	return []refusalControl{
		{"symlinked entrypoint", func(t *testing.T) error {
			dir := scratch(t)
			entry, digest := plainEntry(t, dir, "alias")
			if err := os.Rename(entry, entry+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(entry+".real", entry); err != nil {
				t.Fatal(err)
			}
			_, err := NewCommandFromRequest(entryRequest(dir, entry, digest))
			return err
		}},
		{"entrypoint lost its exec bit, private", func(t *testing.T) error {
			dir := scratch(t)
			entry, digest := plainEntry(t, dir, "noexec-bit")
			if err := os.Chmod(entry, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := NewCommandFromRequest(entryRequest(dir, entry, digest))
			return err
		}},
		{"entrypoint lost its exec bit, pool", func(t *testing.T) error {
			rig := newPoolRig(t)
			entry, digest := plainEntry(t, rig.dir, "noexec-bit")
			if err := os.Chmod(entry, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(entry, digest))
			return err
		}},
		{"missing interpreter, private", func(t *testing.T) error {
			dir := scratch(t)
			f := newEntryFixture(t, dir)
			if err := os.Remove(f.interpreter); err != nil {
				t.Fatal(err)
			}
			_, err := NewCommandFromRequest(entryRequest(dir, f.script, f.digest))
			return err
		}},
		{"missing interpreter, pool", func(t *testing.T) error {
			rig := newPoolRig(t)
			f := newEntryFixture(t, rig.dir)
			if err := os.Remove(f.interpreter); err != nil {
				t.Fatal(err)
			}
			_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(f.script, f.digest))
			return err
		}},
		{"copy hash mismatch after open", copyMismatchAfterOpen},
		{"cached image that does not hash to its name", tamperedCachedImage},
	}
}

func copyMismatchAfterOpen(t *testing.T) error {
	rig := newPoolRig(t)
	f := newEntryFixture(t, rig.dir)
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointSourceOpened {
			f.rewriteInPlace(t)
		}
	}
	_, err := rig.pool.Bind(context.Background(), rig.ns("a"), plainRequest(f.script, f.digest))
	if err == nil || !strings.Contains(err.Error(), "does not match admission") {
		t.Fatalf("a cold bind of a rewritten original gave %v", err)
	}
	return err
}

func tamperedCachedImage(t *testing.T) error {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "tampered-image")
	rig.binder("a", plainRequest(entry, digest))
	image := rig.pool.imagePath(digest)
	if err := os.Chmod(image, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, []byte("tampered cache"), 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := rig.pool.Bind(context.Background(), rig.ns("b"), plainRequest(entry, digest))
	if err == nil || !strings.Contains(err.Error(), "verify custody image") {
		t.Fatalf("a tampered cached image gave %v", err)
	}
	return err
}

// TestOnlyTheEntrypointCarriesTheAdmittedChange are the controls: faults that
// are not "the admitted path changed" stay refusals, including the ones whose
// causes carry an internal change tag below the entry boundary.
func TestOnlyTheEntrypointCarriesTheAdmittedChange(t *testing.T) {
	for _, control := range refusalControls() {
		t.Run(control.name, func(t *testing.T) {
			err := control.run(t)
			if err == nil || errors.Is(err, ErrAdmittedExecutableChanged) || wholeErrorIsAdmittedChange(err) {
				t.Fatalf("got %v; want a refusal that is not an admitted change", err)
			}
		})
	}
}
