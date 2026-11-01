package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const maxFrameSize = 16 * 1024

type childFrame struct {
	Kind     string   `json:"kind"`
	Protocol int      `json:"protocol"`
	RootID   string   `json:"root_id"`
	Epoch    string   `json:"epoch"`
	Attempt  uint64   `json:"attempt"`
	BuildID  string   `json:"build_id"`
	Host     Identity `json:"host"`
	Child    Identity `json:"child"`
}

type childEvent struct {
	frame childFrame
	err   error
}

// channel owns pollable pipe ends and one joined reader. Only its owner writes;
// events transfer immutable data and never mutate host policy or durable state.
type channel struct {
	reader    *os.File
	writer    *os.File
	events    chan childEvent
	done      chan struct{}
	stop      chan struct{}
	once      sync.Once
	closeErr  error
	writeOnce sync.Once
	writeErr  error
}

func newChannel(reader, writer *os.File) *channel {
	c := &channel{reader: reader, writer: writer, events: make(chan childEvent, 4), done: make(chan struct{}), stop: make(chan struct{})}
	go c.read()
	return c
}

func (c *channel) read() {
	defer close(c.done)
	input := bufio.NewReader(c.reader)
	for {
		var frame childFrame
		err := readFrame(input, &frame)
		select {
		case c.events <- childEvent{frame: frame, err: err}:
		case <-c.stop:
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *channel) next(ctx context.Context) (childFrame, error) {
	select {
	case event := <-c.events:
		return event.frame, event.err
	case <-ctx.Done():
		return childFrame{}, ctx.Err()
	}
}

func (c *channel) close() error {
	c.once.Do(func() {
		close(c.stop)
		c.closeErr = errors.Join(c.reader.Close(), c.stopSending())
		<-c.done
	})
	return c.closeErr
}

func (c *channel) stopSending() error {
	c.writeOnce.Do(func() { c.writeErr = c.writer.Close() })
	return c.writeErr
}

func readFrame[T childFrame | wireRequest | wireResponse](input *bufio.Reader, frame *T) error {
	var data []byte
	for len(data) <= maxFrameSize {
		part, err := input.ReadSlice('\n')
		data = append(data, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return err
		}
		if len(data) > maxFrameSize {
			break
		}
		return task.DecodeStrict(data, frame)
	}
	return fmt.Errorf("%w: oversized host frame", task.ErrControlRecordTooBig)
}

func writeFrame[T childFrame | wireRequest | wireResponse](writer io.Writer, frame T) error {
	data, err := task.MarshalCanonical(frame)
	if err != nil {
		return err
	}
	if len(data) > maxFrameSize {
		return task.ErrControlRecordTooBig
	}
	_, err = writer.Write(data)
	return err
}
