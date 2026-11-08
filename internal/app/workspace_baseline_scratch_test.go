package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

const baselineScratchPrefix = "delegate-turn-baseline-"

func countBaselineScratch(parent string) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), baselineScratchPrefix) {
			count++
		}
	}
	return count
}

func TestManagedTurnBaselineScratchUsesSelectedTemporaryRoot(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	turn := lineage.beginTurn(t, resource, registration)
	selected, ambient := supervisorfixture.CanonicalTempDir(t), supervisorfixture.CanonicalTempDir(t)
	t.Setenv("TMPDIR", ambient)
	environment := append(slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "TMPDIR=") }), "TMPDIR="+selected)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDirectory := supervisorfixture.CanonicalTempDir(t)
	witness := filepath.Join(shimDirectory, "scratch-observations")
	script := `#!/bin/sh
for place in selected ambient; do
 case $place in selected) parent=$WITNESS_SELECTED ;; ambient) parent=$WITNESS_AMBIENT ;; esac
 for scratch in "$parent"/delegate-turn-baseline-*; do
  if [ -d "$scratch" ]; then printf '%s\n' "$place" >> "$WITNESS_RECORD"; fi
 done
done
exec "$WITNESS_GIT" "$@"
`
	if err = os.WriteFile(filepath.Join(shimDirectory, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	environment = append(slices.DeleteFunc(environment, func(entry string) bool { return strings.HasPrefix(entry, "PATH=") }), "PATH="+shimDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WITNESS_SELECTED="+selected, "WITNESS_AMBIENT="+ambient, "WITNESS_RECORD="+witness, "WITNESS_GIT="+realGit)
	if err = lineage.observeBaseline(turn.request, environment); err != nil {
		t.Fatalf("baseline with a selected temporary root: %v", err)
	}
	observed, err := os.ReadFile(witness)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(observed), "selected\n") == 0 {
		t.Fatal("baseline scratch was never observed under the selected temporary root")
	}
	if strings.Contains(string(observed), "ambient\n") {
		t.Fatalf("baseline scratch appeared under the process temporary root: %s", observed)
	}
	for _, parent := range []string{selected, ambient} {
		if count := countBaselineScratch(parent); count != 0 {
			t.Fatalf("baseline scratch left %d entries under %q", count, parent)
		}
	}
}
