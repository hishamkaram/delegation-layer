package contract

import (
	"errors"
	"fmt"
	"testing"
)

var errObservation = errors.New("observation failed")

func canonical(path string) PathObservation {
	return PathObservation{Path: path, Canonical: path}
}

func mustRoot(t *testing.T, path string) StateRoot {
	t.Helper()
	root, err := NewStateRoot(canonical(path))
	if err != nil {
		t.Fatalf("NewStateRoot(%q) error = %v", path, err)
	}
	return root
}

func TestNewStateRootRejectsRootsWithoutCanonicalSpelling(t *testing.T) {
	cases := map[string]PathObservation{
		"zero observation":   {},
		"relative spelling":  canonical("relative"),
		"unclean spelling":   canonical("/state/../state"),
		"nul spelling":       canonical("/state\x00"),
		"alias spelling":     {Path: "/alias/state", Canonical: "/real/state"},
		"observation failed": {Path: "/state", Err: errObservation},
		"failed with spelling": {
			Path: "/state", Canonical: "/state", Err: errObservation,
		},
		"empty observation": {Path: "/state"},
	}
	for name, observation := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewStateRoot(observation)
			if !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("NewStateRoot() error = %v, want ErrProfileUnavailable", err)
			}
			if got, want := err.Error(), ErrProfileUnavailable.Error()+": state root is not canonical"; got != want {
				t.Fatalf("NewStateRoot() message = %q, want %q", got, want)
			}
		})
	}
}

func TestZeroStateRootCannotValidateWritableTrees(t *testing.T) {
	var root StateRoot
	err := root.CheckWritable(canonical("/work"))
	if !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("zero state root accepted a writable tree: %v", err)
	}
}

func TestStateRootCheckWritableUsesSuppliedObservation(t *testing.T) {
	root := mustRoot(t, "/state")
	cases := map[string]PathObservation{
		"alias spelling":     {Path: "/alias/work", Canonical: "/real/work"},
		"observation failed": {Path: "/work", Err: errObservation},
	}
	for name, observation := range cases {
		t.Run(name, func(t *testing.T) {
			err := root.CheckWritable(observation)
			if !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("CheckWritable() error = %v, want ErrProfileUnavailable", err)
			}
			want := fmt.Sprintf("%v: provider writable root is not canonical: %s", ErrProfileUnavailable, observation.Path)
			if got := err.Error(); got != want {
				t.Fatalf("CheckWritable() message = %q, want %q", got, want)
			}
		})
	}
}

func TestStateRootCheckWritableRejectsOverlapInEitherDirection(t *testing.T) {
	root := mustRoot(t, "/data/state")
	for _, writable := range []string{"/data/state", "/data/state/cache", "/data", "/"} {
		t.Run(writable, func(t *testing.T) {
			err := root.CheckWritable(canonical(writable))
			if !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("CheckWritable(%q) error = %v, want ErrProfileUnavailable", writable, err)
			}
			want := fmt.Sprintf("%v: state root overlaps provider-writable tree %s", ErrProfileUnavailable, writable)
			if got := err.Error(); got != want {
				t.Fatalf("CheckWritable(%q) message = %q, want %q", writable, got, want)
			}
		})
	}
}

func TestStateRootCheckWritableAcceptsDisjointTrees(t *testing.T) {
	root := mustRoot(t, "/data/state")
	for _, writable := range []string{"/data/state-cache", "/data/other", "/work", "/data/statefulness/cache"} {
		if err := root.CheckWritable(canonical(writable)); err != nil {
			t.Errorf("CheckWritable(%q) error = %v, want nil", writable, err)
		}
	}
}

func TestStateRootCheckWritableReportsNonCanonicalBeforeOverlap(t *testing.T) {
	root := mustRoot(t, "/data/state")
	overlappingAlias := PathObservation{Path: "/data/state/cache", Canonical: "/elsewhere/cache"}
	err := root.CheckWritable(overlappingAlias)
	want := fmt.Sprintf("%v: provider writable root is not canonical: /data/state/cache", ErrProfileUnavailable)
	if err == nil || err.Error() != want {
		t.Fatalf("CheckWritable() error = %v, want %q", err, want)
	}
}
