//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrepareUsesInstalledExecutableWithoutReadingItsContents(t *testing.T) {
	// TestMain owns this one source-built installation. This sequential test
	// restores its mode before another test or TestMain's cleanup can use it.
	info, err := os.Stat(fixtureBinPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if restoreErr := os.Chmod(fixtureBinPath, info.Mode().Perm()); restoreErr != nil {
			t.Error(restoreErr)
		}
	})
	if err = os.Chmod(fixtureBinPath, 0o111); err != nil {
		t.Fatal(err)
	}
	c := newCase(t)
	c.prepare(t)
	data, err := os.ReadFile(filepath.Join(c.root, "tasks", c.id, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta task.MetaRecord
	if err = task.DecodeStrict(data, &meta); err != nil {
		t.Fatal(err)
	}
	selected, err := filepath.EvalSymlinks(fixtureBinPath)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ProviderExecutable != selected || meta.PublisherBuild != meta.ProviderVersion {
		t.Fatalf("fixture metadata lost installed selection or diagnostic version: %q %q %q", meta.ProviderExecutable, meta.PublisherBuild, meta.ProviderVersion)
	}
}
