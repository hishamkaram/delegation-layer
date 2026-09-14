package taskdir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func (td *TaskDir) ownedRunner() (*leaseState, error) {
	td.mu.Lock()
	defer td.mu.Unlock()
	if td.closed || td.runnerLease == nil || td.runnerLease.state == nil {
		return nil, task.ErrInvalidPermit
	}
	state := td.runnerLease.state
	state.mu.Lock()
	if state.released || state.permit == nil || !state.permit.consumed {
		state.mu.Unlock()
		return nil, task.ErrInvalidPermit
	}
	return state, nil // caller unlocks
}

func (td *TaskDir) RecordStarted(diagnosticNanos int64) error {
	state, err := td.ownedRunner()
	if err != nil {
		return err
	}
	defer state.mu.Unlock()
	if state.sealed {
		return task.ErrEvidenceFault
	}
	_, _, _, spec, meta, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return err
	}
	rec := task.ProviderStartedRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: meta, StartedAt: timestamp(), DiagnosticNanos: diagnosticNanos}
	if err = task.ValidateProviderStartedRecord(&rec); err != nil {
		return err
	}
	return td.commitRecord("provider.started.json", rec)
}

type rawWriter struct {
	mu       sync.Mutex
	file     *os.File
	state    *leaseState
	injector FaultInjector
	closed   bool
	failed   error
}

func (w *rawWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.injector != nil {
		if err := w.injector.OnRawWrite(w.file.Name()); err != nil {
			w.failed = errors.Join(w.failed, err)
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.failed = errors.Join(w.failed, err)
	return n, err
}

func (w *rawWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.failed
	}
	w.closed = true
	err := w.finish()
	w.failed = errors.Join(w.failed, err, w.file.Close())
	w.state.mu.Lock()
	w.state.activeWriters--
	w.state.poisoned = errors.Join(w.state.poisoned, w.failed)
	w.state.mu.Unlock()
	return w.failed
}

func (w *rawWriter) finish() error {
	if w.injector != nil {
		if err := w.injector.OnRawBarrier(w.file.Name()); err != nil {
			return err
		}
	}
	if err := platformBarrierFile(w.file); err != nil {
		return err
	}
	if w.injector != nil {
		return w.injector.OnRawClose(w.file.Name())
	}
	return nil
}

// OpenRawWriter is only available to a consumed live Start permit. It never
// truncates old bytes; any write/flush/close failure permanently prevents sealing.
func (td *TaskDir) OpenRawWriter(name string) (io.WriteCloser, error) {
	return td.openRawWriter(name)
}

func (td *TaskDir) openRawWriter(name string) (*rawWriter, error) {
	if name != "stdout" && name != "stderr" {
		return nil, task.ErrEvidenceFault
	}
	state, err := td.ownedRunner()
	if err != nil {
		return nil, err
	}
	defer state.mu.Unlock()
	if state.sealed || state.poisoned != nil {
		return nil, task.ErrEvidenceFault
	}
	f, err := td.store.openFile(filepath.Join(td.Dir, "raw", name), unix.O_WRONLY|unix.O_APPEND)
	if err != nil {
		return nil, err
	}
	state.activeWriters++
	return &rawWriter{file: f, state: state, injector: td.store.faultInjector}, nil
}

func (td *TaskDir) WriteRawFrom(stdout, stderr io.Reader) error {
	for _, stream := range []struct {
		name   string
		reader io.Reader
	}{{"stdout", stdout}, {"stderr", stderr}} {
		if stream.reader == nil {
			continue
		}
		w, err := td.openRawWriter(stream.name)
		if err != nil {
			return err
		}
		err = copyChecked(w, stream.reader)
		w.mu.Lock()
		w.failed = errors.Join(w.failed, err)
		w.mu.Unlock()
		err = w.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (td *TaskDir) WriteRawFiles(stdout, stderr string) error {
	return td.WriteRawFrom(strings.NewReader(stdout), strings.NewReader(stderr))
}

func (s *Store) fileDigest(path string) (string, int64, error) {
	f, err := s.openFile(path, unix.O_RDONLY)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (td *TaskDir) rawManifest() ([]task.RawManifestEntry, string, error) {
	names, err := td.admittedRawNames()
	if err != nil {
		return nil, "", err
	}
	slices.Sort(names)
	manifest := make([]task.RawManifestEntry, 0, len(names))
	for _, name := range names {
		path := "raw/" + name
		digest, size, digestErr := td.store.fileDigest(filepath.Join(td.Dir, path))
		if digestErr != nil {
			return nil, "", digestErr
		}
		manifest = append(manifest, task.RawManifestEntry{Path: path, Size: size, SHA256: digest})
	}
	data, err := task.MarshalCanonical(manifest)
	if err != nil {
		return nil, "", err
	}
	return manifest, task.ComputeSHA256(data), nil
}

func (td *TaskDir) Seal(invocation string, exitCode int, exitErr string, predicate task.PredicateRef) (*task.ProviderExitRecord, error) {
	state, err := td.ownedRunner()
	if err != nil {
		return nil, err
	}
	defer state.mu.Unlock()
	if state.activeWriters != 0 || state.poisoned != nil {
		return nil, fmt.Errorf("%w: active or failed raw capture", task.ErrEvidenceFault)
	}
	_, _, meta, specHash, metaHash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	if len(meta.OutputArtifacts) > 0 && !state.artifactsImported {
		return nil, fmt.Errorf("%w: output artifacts were not imported", task.ErrEvidenceFault)
	}
	if !predicate.Equal(meta.Predicate) {
		return nil, task.ErrIncompatiblePredicate
	}
	existing, err := td.readSeal(predicate, specHash, metaHash)
	if err == nil {
		state.sealed = true
		if err = td.acknowledgeRecord("provider.exit"); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, task.ErrNoSeal) {
		return nil, err
	}
	if err = td.barrierRaw(); err != nil {
		return nil, err
	}
	manifest, digest, err := td.rawManifest()
	if err != nil {
		return nil, err
	}
	rec := task.ProviderExitRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: specHash, MetaSHA256: metaHash, InvocationState: invocation, ExitCode: exitCode, Error: exitErr, Predicate: predicate, RawManifest: manifest, ManifestSHA256: digest, ClosedAt: timestamp()}
	if err = task.ValidateProviderExitRecord(&rec); err != nil {
		return nil, err
	}
	// Prevent any later writer even when seal publication becomes uncertain.
	state.sealed = true
	if err = td.commitRecord("provider.exit", rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (td *TaskDir) barrierRaw() error {
	inj := td.store.faultInjector
	names, err := td.admittedRawNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		path := filepath.Join(td.Dir, "raw", name)
		f, err := td.store.openFile(path, unix.O_RDONLY)
		if err != nil {
			return err
		}
		if inj != nil {
			if err = inj.OnRawBarrier(path); err != nil {
				closeFileQuietly(f)
				return err
			}
		}
		err = errors.Join(platformBarrierFile(f), f.Close())
		if err != nil {
			return err
		}
	}
	dir := filepath.Join(td.Dir, "raw")
	if inj != nil {
		if err := inj.OnRawDirBarrier(dir); err != nil {
			return err
		}
	}
	return td.store.barrierDir(dir)
}

func (td *TaskDir) readSeal(predicate task.PredicateRef, spec, meta string) (*task.ProviderExitRecord, error) {
	seal, err := td.readSealRecord(predicate, spec, meta)
	if err != nil && !errors.Is(err, task.ErrNoSeal) {
		return nil, fmt.Errorf("%w: %w", task.ErrEvidenceFault, err)
	}
	return seal, err
}

func (td *TaskDir) readSealRecord(predicate task.PredicateRef, spec, meta string) (*task.ProviderExitRecord, error) {
	var seal task.ProviderExitRecord
	if err := td.store.readRecord(filepath.Join(td.Dir, "provider.exit"), &seal); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, task.ErrNoSeal
		}
		return nil, err
	}
	if err := task.ValidateProviderExitRecord(&seal); err != nil {
		return nil, err
	}
	if !seal.Predicate.Equal(predicate) {
		return nil, task.ErrIncompatiblePredicate
	}
	if seal.RootID != td.store.RootID || seal.TaskID != td.TaskID || seal.SpecSHA256 != spec || seal.MetaSHA256 != meta {
		return nil, task.ErrIdentityMismatch
	}
	startExists, err := td.store.exists(filepath.Join(td.Dir, "provider.start"))
	if err != nil {
		return nil, err
	}
	if !startExists {
		return nil, task.ErrEvidenceFault
	}
	if err := td.validateSealedFiles(&seal); err != nil {
		return nil, err
	}
	return &seal, nil
}

func (td *TaskDir) validateSealedFiles(seal *task.ProviderExitRecord) error {
	if err := td.validateManifestDeclarations(seal.RawManifest); err != nil {
		return err
	}
	for _, entry := range seal.RawManifest {
		digest, size, e := td.store.fileDigest(filepath.Join(td.Dir, entry.Path))
		if e != nil {
			return e
		}
		if digest != entry.SHA256 || size != entry.Size {
			return task.ErrEvidenceFault
		}
	}
	return nil
}
