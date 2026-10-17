package policy

import (
	"slices"
	"testing"
)

func TestSnapshotRejectsChangedAddedAndRemovedSource(t *testing.T) {
	before := []Source{{Path: "removed.go", SHA256: "old"}, {Path: "changed.go", SHA256: "old"}}
	after := []Source{{Path: "changed.go", SHA256: "new"}, {Path: "added.go", SHA256: "new"}}
	want := []string{"added.go", "changed.go", "removed.go"}
	if got := ChangedSources(before, after); !slices.Equal(got, want) {
		t.Fatalf("mixed source snapshot accepted: got %v, want %v", got, want)
	}
}

func TestCollectedFactsDoNotChangeTheSourceSnapshot(t *testing.T) {
	before := []Source{{Path: "value.go", SHA256: "same", Kind: "code", Lines: 10}}
	after := slices.Clone(before)
	after[0].Module = "go:domain"
	after[0].Functions = []Function{{Name: "value", Line: 1}}
	if changed := ChangedSources(before, after); len(changed) != 0 {
		t.Fatalf("collector observations changed source identity: %v", changed)
	}
}
