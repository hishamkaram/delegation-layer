package execution

import (
	"errors"
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type capture struct {
	name   string
	reader *os.File
	writer *os.File
	raw    io.WriteCloser
	done   chan error
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
	return &capture{name: name, reader: r, writer: w, raw: raw, done: make(chan error, 1)}, nil
}

func (c *capture) discard() error {
	return errors.Join(c.writer.Close(), c.reader.Close(), c.raw.Close())
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
		dst = observedWriter{raw: c.raw, observer: opts.Identity}
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
	c.done <- err
}
