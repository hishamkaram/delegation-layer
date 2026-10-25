package control

import (
	"os"
	"slices"
	"testing"
)

func TestDefaultEnvironmentFromUsesSelectedSnapshot(t *testing.T) {
	t.Setenv("LANG", "issue91-caller")
	selected := []string{"LANG=issue91-selected", "DELEGATE_UNRELATED_TEST_VALUE=drop"}
	filtered := DefaultEnvironmentFrom(selected)
	if !slices.Equal(filtered, []string{"LANG=issue91-selected"}) {
		t.Fatal("control filtering did not select only the supplied control value")
	}
	filtered[0] = "LANG=changed"
	if selected[0] != "LANG=issue91-selected" || os.Getenv("LANG") != "issue91-caller" {
		t.Fatal("control filtering changed its input or the caller")
	}
	for _, empty := range [][]string{nil, {}} {
		if result := DefaultEnvironmentFrom(empty); result == nil || len(result) != 0 {
			t.Fatal("empty control input inherited caller values")
		}
	}
}
