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
	return e.failure
}

func (td *TaskDir) openValidatedEvidence(seal *task.ProviderExitRecord) (*sealedEvidence, error) {
	evidence := &sealedEvidence{streams: make(map[predicate.Stream]sealedStream)}
	for _, entry := range seal.RawManifest {
		key := predicate.Stdout
		if entry.Path == "raw/stderr" {
			key = predicate.Stderr
		} else if entry.Path != "raw/stdout" {
			return nil, errors.Join(task.ErrEvidenceFault, evidence.Close())
		}
		f, err := td.store.openFile(filepath.Join(td.Dir, entry.Path), os.O_RDONLY)
		if err != nil {
			return nil, errors.Join(task.ErrEvidenceFault, err, evidence.Close())
		}
		evidence.streams[key] = sealedStream{file: f, size: entry.Size}
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil || n != entry.Size || hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
			return nil, errors.Join(task.ErrEvidenceFault, err, evidence.Close())
		}
	}
	return evidence, nil
}
