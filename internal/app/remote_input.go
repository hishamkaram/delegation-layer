package app

import (
	"context"
	"errors"
	"io"
	"time"
)

// remoteInput owns only timeout interruption of this dedicated control
// process's input. The mutation context does not depend on channel EOF.
type remoteInput struct {
	parent context.Context
	input  io.ReadCloser
	idle   time.Duration
}

func newRemoteInput(ctx context.Context, input io.Reader, idle time.Duration) (*remoteInput, error) {
	closer, ok := input.(io.ReadCloser)
	if !ok {
		return nil, errors.New("remote control requires closeable input")
	}
	if idle <= 0 {
		return nil, errors.New("remote input idle deadline must be positive")
	}
	return &remoteInput{parent: ctx, input: closer, idle: idle}, nil
}

func (r *remoteInput) Read(data []byte) (int, error) {
	ctx, cancel := context.WithTimeout(r.parent, r.idle)
	defer cancel()
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- r.input.Close() })
	n, err := r.input.Read(data)
	if !stop() {
		err = errors.Join(err, ctx.Err(), <-closed)
	}
	return n, err
}

// remoteOutput owns interruption of a response blocked by an unread channel.
// Timing out the response never cancels a mutation which has already settled.
type remoteOutput struct {
	parent context.Context
	output io.WriteCloser
	idle   time.Duration
}

func newRemoteOutput(ctx context.Context, output io.Writer, idle time.Duration) (*remoteOutput, error) {
	closer, ok := output.(io.WriteCloser)
	if !ok {
		return nil, errors.New("remote control requires closeable output")
	}
	if idle <= 0 {
		return nil, errors.New("remote output idle deadline must be positive")
	}
	return &remoteOutput{parent: ctx, output: closer, idle: idle}, nil
}

func (w *remoteOutput) Write(data []byte) (int, error) {
	ctx, cancel := context.WithTimeout(w.parent, w.idle)
	defer cancel()
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- w.output.Close() })
	n, err := w.output.Write(data)
	if !stop() {
		err = errors.Join(err, ctx.Err(), <-closed)
	}
	return n, err
}
