package task

import (
	"strings"
	"testing"
)

func TestLifetimeCustodyPlacementCloneIsNilPreservingSnapshot(t *testing.T) {
	var absent *LifetimeCustodyPlacement
	if absent.Clone() != nil {
		t.Fatal("nil placement clone is not nil")
	}
	original := validCustodyPlacement()
	clone := original.Clone()
	if clone == original {
		t.Fatal("placement clone shares the original pointer")
	}
	if *clone != *original {
		t.Fatalf("placement clone = %+v, want %+v", clone, original)
	}
	original.Version = 2
	original.PoolRoot = "/elsewhere"
	original.PoolID = strings.Repeat("c", 64)
	if want := validCustodyPlacement(); *clone != *want {
		t.Fatalf("clone changed with its original: %+v, want %+v", clone, want)
	}
	clone.PoolID = strings.Repeat("d", 64)
	if original.PoolID != strings.Repeat("c", 64) {
		t.Fatalf("original changed with its clone: %+v", original)
	}
}
