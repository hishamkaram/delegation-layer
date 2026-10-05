package execution

import (
	"errors"
	"io"
	"os"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type capture struct {
	name    string
	observe *observedReader
	framer  *providerjson.Framer
	reader  *os.File
	writer  *os.File
	raw     io.WriteCloser
	done    chan error
}

func newCapture(td *taskdir.TaskDir, name string) (*capture, error) {
	raw, err := td.OpenRawWriter(name)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	var observe *observedReader
	if name == "stdout" {
		file, openErr := td.OpenRawObserverReader()
		err = openErr
		if openErr == nil {
			observe = &observedReader{file: file}
		}
		if err != nil {
			return nil, errors.Join(err, r.Close(), w.Close(), raw.Close())
		}
	}
	return &capture{name: name, observe: observe, reader: r, writer: w, raw: raw, done: make(chan error, 1)}, nil
}

func (c *capture) discard() error {
	return errors.Join(c.writer.Close(), c.reader.Close(), c.raw.Close(), c.closeObserver())
}

type observedWriter struct {
	raw      io.Writer
	observer IdentityObserver
}

func (w observedWriter) Write(p []byte) (int, error) {
	n, err := w.raw.Write(p)
	if n > 0 {
		w.observer.Observe(p[:n])
	}
	return n, err
}

func (c *capture) run(opts Options) {
	var dst io.Writer = c.raw
	if c.name == "stdout" && opts.Identity != nil {
		if observer, ok := opts.Identity.(RecordIdentityObserver); ok {
			c.framer = providerjson.NewFramer(func(start, end int64, _ uint64) error {
				return observer.ObserveRecord(providerjson.Value{Reader: recordReader{ReaderAt: c.observe}, Start: start, End: end})
			})
			dst = recordWriter{raw: c.raw, framer: c.framer}
		} else {
			dst = observedWriter{raw: c.raw, observer: opts.Identity}
		}
	}
	var err error
	if opts.Hooks.Capture != nil {
		err = opts.Hooks.Capture(c.name, dst, c.reader)
	} else {
		_, err = io.Copy(dst, c.reader)
	}
	if err == nil {
		opts.emit(c.name + "-eof")
	} else {
		// Once capture is faulty no seal is possible, but we still own the pipe.
		// Drain it to natural EOF so a finite child can finish without backpressure
		// or a reader-close-induced broken pipe. Preserve the original failure.
		_, drainErr := io.Copy(io.Discard, c.reader)
		err = errors.Join(err, drainErr)
		if drainErr == nil {
			opts.emit(c.name + "-drained-after-error")
		}
	}
	err = errors.Join(err, c.reader.Close(), c.raw.Close())
	opts.emit(c.name + "-raw-closed")
	if c.framer != nil {
		// Semantic observation faults are decided again from the sealed stream.
		// Reader and recorder failures remain operational through Close/Complete.
		if observationErr := c.framer.Finish(); observationErr != nil {
			opts.emit("stdout-observation-rejected")
		}
	}
	c.done <- err
}

func (c *capture) closeObserver() error {
	if c.observe == nil {
		return nil
	}
	return c.observe.Close()
}

type recordWriter struct {
	raw    io.Writer
	framer *providerjson.Framer
}

func (w recordWriter) Write(p []byte) (int, error) {
	n, err := w.raw.Write(p)
	if n > 0 {
		w.framer.Feed(p[:n])
	}
	return n, err
}

// observedReader preserves operational read failures independently from a
// provider's semantic decision and expires when core observation completes.
type observedReader struct {
	mu    sync.Mutex
	file  *os.File
	fault error
}

func (r *observedReader) ReadAt(p []byte, offset int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.file.ReadAt(p, offset)
	if err != nil {
		r.fault = errors.Join(r.fault, err)
	}
	return n, err
}

func (r *observedReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.fault, r.file.Close())
}

// recordReader exposes reads only; adapters cannot close the core-owned file.
type recordReader struct{ io.ReaderAt }
