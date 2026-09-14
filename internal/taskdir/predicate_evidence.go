package taskdir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type (
	sealedStream struct {
		file *os.File
		size int64
	}
	sealedEvidence struct {
		activeReaders int
		mu            sync.Mutex
		streams       map[predicate.Stream]sealedStream
		named         map[string]sealedStream
		declared      map[string]bool
		closed        bool
		failure       error
	}
	scopedReader struct {
		mu      sync.Mutex
		reader  io.Reader
		active  bool
		failure error
	}
)

func (r *scopedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return 0, os.ErrClosed
	}
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.failure = errors.Join(r.failure, err)
	}
	return n, err
}

func (e *sealedEvidence) Read(key predicate.Stream, consume func(io.Reader) error) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return os.ErrClosed
	}
	stream, ok := e.streams[key]
	return e.readLocked(stream, ok, consume)
}

func (e *sealedEvidence) ReadNamed(name string, consume func(io.Reader) error) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return os.ErrClosed
	}
	stream, present := e.named[name]
	if e.declared[name] && !present && consume != nil {
		e.mu.Unlock()
		return predicate.ErrEvidenceAbsent
	}
	return e.readLocked(stream, present && e.declared[name], consume)
}

// readLocked consumes and releases e.mu; both APIs share reader lifetime and
// poison tracking.
func (e *sealedEvidence) readLocked(stream sealedStream, ok bool, consume func(io.Reader) error) error {
	if !ok || consume == nil {
		e.failure = errors.Join(e.failure, task.ErrEvidenceFault)
		e.mu.Unlock()
		return task.ErrEvidenceFault
	}
	e.activeReaders++
	e.mu.Unlock()
	reader := &scopedReader{reader: io.NewSectionReader(stream.file, 0, stream.size), active: true}
	err := consume(reader)
	reader.mu.Lock()
	reader.active = false
	failure := reader.failure
	reader.mu.Unlock()
	e.mu.Lock()
	e.activeReaders--
	e.failure = errors.Join(e.failure, failure)
	e.mu.Unlock()
	return errors.Join(err, failure)
}

func (e *sealedEvidence) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return e.failure
	}
	e.closed = true
	if e.activeReaders != 0 {
		e.failure = errors.Join(e.failure, task.ErrEvidenceFault)
	}
	for _, stream := range e.streams {
		e.failure = errors.Join(e.failure, stream.file.Close())
	}
	for _, stream := range e.named {
		e.failure = errors.Join(e.failure, stream.file.Close())
	}
	return e.failure
}

func (td *TaskDir) openValidatedEvidence(seal *task.ProviderExitRecord) (*sealedEvidence, error) {
	if err := td.validateManifestDeclarations(seal.RawManifest); err != nil {
		return nil, err
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	evidence := &sealedEvidence{streams: make(map[predicate.Stream]sealedStream), named: make(map[string]sealedStream), declared: make(map[string]bool)}
	for _, output := range meta.OutputArtifacts {
		evidence.declared[output.Name] = true
	}
	for _, entry := range seal.RawManifest {
		if err := td.openEvidenceEntry(evidence, entry); err != nil {
			return nil, errors.Join(task.ErrEvidenceFault, err, evidence.Close())
		}
	}
	return evidence, nil
}

func (td *TaskDir) openEvidenceEntry(evidence *sealedEvidence, entry task.RawManifestEntry) error {
	f, err := td.store.openFile(filepath.Join(td.Dir, entry.Path), os.O_RDONLY)
	if err != nil {
		return err
	}
	stream := sealedStream{file: f, size: entry.Size}
	switch entry.Path {
	case "raw/stdout":
		evidence.streams[predicate.Stdout] = stream
	case "raw/stderr":
		evidence.streams[predicate.Stderr] = stream
	default:
		evidence.named[filepath.Base(entry.Path)] = stream
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil || n != entry.Size || hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	return nil
}
