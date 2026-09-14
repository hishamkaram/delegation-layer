package phase2cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateHarnessConfigRejectsSymlinkedEventsDirectoryComponents(t *testing.T) {
	dir := canonicalTestDir(t)
	fixture := testHarnessFixture(t, dir)

	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(dir, "events-parent-link")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "events-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	finalLink := filepath.Join(dir, "events-final-link")
	if err := os.Symlink(target, finalLink); err != nil {
		t.Fatal(err)
	}

	for name, eventsDirectory := range map[string]string{
		"symlinked parent":          filepath.Join(parentLink, "events"),
		"symlinked final component": finalLink,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := fixture.config
			cfg.EventsDirectory = eventsDirectory
			if err := validateHarnessConfig(cfg); err == nil {
				t.Fatalf("accepted symlinked events directory %q", eventsDirectory)
			}
		})
	}
}

func TestValidateHarnessConfigAllowsMissingCanonicalEventsDirectoryForRecorder(t *testing.T) {
	dir := canonicalTestDir(t)
	fixture := testHarnessFixture(t, dir)
	if _, err := os.Lstat(fixture.config.EventsDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture events directory already exists: %v", err)
	}
	if err := validateHarnessConfig(fixture.config); err != nil {
		t.Fatal(err)
	}
	recorder := newEventRecorder(fixture.config)
	if err := recorder.begin([]string{"delegate"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fixture.config.EventsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("recorder created non-directory events root: %s", fixture.config.EventsDirectory)
	}
}
