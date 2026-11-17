package resultread

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type observedSource struct {
	io.Reader
	onClose func() error
	closes  int
}

func (s *observedSource) Close() error {
	s.closes++
	if s.onClose != nil {
		return s.onClose()
	}
	return nil
}

func saveDescriptor(data string) task.PayloadDescriptor {
	return task.PayloadDescriptor{Basename: "result.txt", Length: int64(len(data)), SHA256: task.ComputeSHA256([]byte(data))}
}

func requireNoSavedOutput(t *testing.T, output string) {
	t.Helper()
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpublished output exists: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(output))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Fatalf("owned stage leaked: %s", entry.Name())
		}
	}
}

func TestSaveCancellationAfterVerificationDoesNotPublish(t *testing.T) {
	data := "complete"
	output := filepath.Join(t.TempDir(), "answer")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &observedSource{Reader: strings.NewReader(data), onClose: func() error { cancel(); return nil }}
	saved, err := Save(ctx, t.TempDir(), output, saveDescriptor(data), source)
	if !errors.Is(err, context.Canceled) || saved != nil || source.closes != 1 {
		t.Fatalf("cancelled result=%+v error=%v closes=%d", saved, err, source.closes)
	}
	requireNoSavedOutput(t, output)
}

func TestSaveCorruptPayloadDoesNotPublish(t *testing.T) {
	output := filepath.Join(t.TempDir(), "answer")
	source := &observedSource{Reader: strings.NewReader("corrupt!")}
	saved, err := Save(t.Context(), t.TempDir(), output, saveDescriptor("complete"), source)
	if !errors.Is(err, task.ErrEvidenceFault) || saved != nil || source.closes != 1 {
		t.Fatalf("corrupt result=%+v error=%v closes=%d", saved, err, source.closes)
	}
	requireNoSavedOutput(t, output)
}

func TestSavePreservesRacingDestination(t *testing.T) {
	output := filepath.Join(t.TempDir(), "answer")
	data := "complete"
	source := &observedSource{Reader: strings.NewReader(data), onClose: func() error { return os.WriteFile(output, []byte("winner"), 0o600) }}
	saved, err := Save(t.Context(), t.TempDir(), output, saveDescriptor(data), source)
	if !errors.Is(err, os.ErrExist) || saved != nil || source.closes != 1 {
		t.Fatalf("racing destination accepted: %+v %v closes%d", saved, err, source.closes)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != "winner" {
		t.Fatal("racing writer changed")
	}
}

func TestSaveRefusesReplacedDestinationParent(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "destination")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "answer")
	data := "answer"
	source := &observedSource{Reader: strings.NewReader(data), onClose: func() error {
		if err := os.Rename(parent, filepath.Join(base, "old")); err != nil {
			return err
		}
		return os.Mkdir(parent, 0o700)
	}}
	saved, err := Save(t.Context(), t.TempDir(), output, saveDescriptor(data), source)
	if !errors.Is(err, task.ErrRequestConflict) || saved != nil || source.closes != 1 {
		t.Fatalf("parent replacement accepted: %+v %v closes%d", saved, err, source.closes)
	}
	for _, path := range []string{parent, filepath.Join(base, "old")} {
		entries, readErr := os.ReadDir(path)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("owned stage leaked: %s %+v %v", path, entries, readErr)
		}
	}
}

func TestSaveSearchOnlyDirectories(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "search-only")
	parent := filepath.Join(ancestor, "output")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(os.Chmod(ancestor, 0o700), os.Chmod(parent, 0o700)); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(ancestor, 0o111); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o300); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "answer")
	data := "verified through search-only directories"
	source := &observedSource{Reader: strings.NewReader(data)}
	saved, err := Save(t.Context(), t.TempDir(), output, saveDescriptor(data), source)
	if err != nil || saved == nil || source.closes != 1 {
		t.Fatalf("valid search-only destination: %+v %v closes%d", saved, err, source.closes)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != data {
		t.Fatalf("saved bytes: %q %v", got, err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved mode: %v %v", info, err)
	}
}

func TestSaveClosesSourceWhenDestinationCannotOpen(t *testing.T) {
	source := &observedSource{Reader: strings.NewReader("complete")}
	output := filepath.Join(t.TempDir(), "absent", "answer")
	saved, err := Save(t.Context(), t.TempDir(), output, saveDescriptor("complete"), source)
	if !errors.Is(err, os.ErrNotExist) || saved != nil || source.closes != 1 {
		t.Fatalf("source ownership lost: %+v %v closes%d", saved, err, source.closes)
	}
}

func TestSaveNilContextClosesSource(t *testing.T) {
	source := &observedSource{Reader: strings.NewReader("complete")}
	output := filepath.Join(t.TempDir(), "answer")
	// An unset typed boundary value exercises defensive nil handling.
	var invalidContext context.Context
	saved, err := Save(invalidContext, t.TempDir(), output, saveDescriptor("complete"), source)
	if !errors.Is(err, context.Canceled) || saved != nil || source.closes != 1 {
		t.Fatalf("nil context: %+v %v closes%d", saved, err, source.closes)
	}
	requireNoSavedOutput(t, output)
}

func TestSaveSourceCloseFailureDoesNotPublish(t *testing.T) {
	closeErr := errors.New("source close failed")
	source := &observedSource{Reader: strings.NewReader("complete"), onClose: func() error { return closeErr }}
	output := filepath.Join(t.TempDir(), "answer")
	saved, err := Save(t.Context(), t.TempDir(), output, saveDescriptor("complete"), source)
	if !errors.Is(err, closeErr) || saved != nil || source.closes != 1 {
		t.Fatalf("close failure published: %+v %v closes%d", saved, err, source.closes)
	}
	requireNoSavedOutput(t, output)
}

func TestSavedCleanupFailureRetainsPublicationFact(t *testing.T) {
	output := filepath.Join(t.TempDir(), "answer")
	descriptor := saveDescriptor("complete")
	root := t.TempDir()
	destination, err := openResultDestination(t.Context(), root, output)
	if err != nil {
		t.Fatal(err)
	}
	if err = copyVerified(t.Context(), bytes.NewBufferString("complete"), descriptor, destination.stage); err != nil {
		t.Fatal(err)
	}
	if err = destination.stage.Close(); err != nil {
		t.Fatal(err)
	}
	destination.stage = nil
	saved, err := destination.publish(t.Context(), root, output, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	// Closing the owned descriptor induces a real post-link stage-cleanup failure.
	if err = destination.directory.Close(); err != nil {
		t.Fatal(err)
	}
	cleanupErr := destination.cleanup()
	if cleanupErr == nil || saved == nil || saved.Path != output || saved.Length != 8 || saved.SHA256 != descriptor.SHA256 {
		t.Fatalf("saved fact lost: %+v %v", saved, cleanupErr)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != "complete" {
		t.Fatalf("published bytes lost: %v", err)
	}
}
