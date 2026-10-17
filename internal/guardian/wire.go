package guardian

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const lifetimeProtocolVersion = 1

const lifetimeControlTimeout = 10 * time.Second

const (
	lifetimeMessageHello   = "hello"
	lifetimeMessageBegin   = "begin"
	lifetimeMessageStarted = "started"
	lifetimeMessageFailed  = "failed"
	lifetimeMessageFinish  = "finish"
	lifetimeMessageStop    = "stop"
	lifetimeMessageClose   = "close"
)

type lifetimeMessage struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`
	TaskID   string `json:"task_id"`
	Sequence uint64 `json:"sequence,omitempty"`
	PID      int    `json:"pid,omitempty"`
}

type lifetimeReply struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`
	Sequence uint64 `json:"sequence,omitempty"`
	Accepted bool   `json:"accepted"`
	Code     string `json:"code,omitempty"`
}

type lifetimeCodec struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
	closed bool
}

func newLifetimeCodec(conn io.ReadWriteCloser) (*lifetimeCodec, error) {
	if conn == nil {
		return nil, task.ErrEvidenceFault
	}
	network, ok := conn.(net.Conn)
	if !ok {
		return nil, task.ErrEvidenceFault
	}
	return &lifetimeCodec{conn: network, reader: bufio.NewReaderSize(network, int(task.MaxControlRecordSize)+1)}, nil
}

func (c *lifetimeCodec) callContext(ctx context.Context, message lifetimeMessage) error {
	if c == nil {
		return task.ErrEvidenceFault
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return osClosedError()
	}
	if err := validateLifetimeMessage(message); err != nil {
		return err
	}
	clear, err := c.setDeadline(ctx)
	if err != nil {
		return err
	}
	defer clear()
	if err = writeLifetimeFrame(c, message); err != nil {
		return err
	}
	var reply lifetimeReply
	if err = readLifetimeFrame(c, &reply); err != nil {
		return err
	}
	if err := validateLifetimeReply(reply, message); err != nil {
		return err
	}
	if !reply.Accepted {
		return errors.New("task lifetime request refused: " + reply.Code)
	}
	return nil
}

func (c *lifetimeCodec) receive() (lifetimeMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeControlTimeout)
	defer cancel()
	return c.receiveContext(ctx)
}

func (c *lifetimeCodec) receiveContext(ctx context.Context) (lifetimeMessage, error) {
	if c == nil {
		return lifetimeMessage{}, task.ErrEvidenceFault
	}
	clear, err := c.setDeadline(ctx)
	if err != nil {
		return lifetimeMessage{}, err
	}
	defer clear()
	var message lifetimeMessage
	if err = readLifetimeFrame(c, &message); err != nil {
		return lifetimeMessage{}, err
	}
	return message, validateLifetimeMessage(message)
}

func (c *lifetimeCodec) reply(message lifetimeMessage, accepted bool, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeControlTimeout)
	defer cancel()
	return c.replyContext(ctx, message, accepted, code)
}

func (c *lifetimeCodec) replyContext(ctx context.Context, message lifetimeMessage, accepted bool, code string) error {
	if c == nil {
		return task.ErrEvidenceFault
	}
	clear, err := c.setDeadline(ctx)
	if err != nil {
		return err
	}
	defer clear()
	reply := lifetimeReply{Version: lifetimeProtocolVersion, Type: message.Type, Sequence: message.Sequence, Accepted: accepted, Code: code}
	if accepted {
		reply.Code = ""
	}
	return writeLifetimeFrame(c, reply)
}

func (c *lifetimeCodec) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

func (c *lifetimeCodec) setDeadline(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return nil, task.ErrEvidenceFault
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	return func() { clearLifetimeDeadline(c.conn) }, nil
}

func clearLifetimeDeadline(conn net.Conn) {
	if conn == nil {
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}
}

type lifetimeFrame interface {
	lifetimeMessage | lifetimeReply
}

func writeLifetimeFrame[T lifetimeFrame](c *lifetimeCodec, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if int64(len(data)+1) > task.MaxControlRecordSize {
		return task.ErrEvidenceFault
	}
	data = append(data, '\n')
	for len(data) > 0 {
		written, writeErr := c.conn.Write(data)
		if writeErr != nil {
			return writeErr
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func readLifetimeFrame[T lifetimeFrame](c *lifetimeCodec, value *T) error {
	data, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return task.ErrEvidenceFault
	}
	if err != nil {
		return err
	}
	if len(data) == 0 || int64(len(data)) > task.MaxControlRecordSize {
		return task.ErrEvidenceFault
	}
	return task.DecodeStrict(data[:len(data)-1], value)
}

func validateLifetimeMessage(message lifetimeMessage) error {
	if message.Version != lifetimeProtocolVersion || task.ValidateTaskID(message.TaskID) != nil {
		return task.ErrEvidenceFault
	}
	switch message.Type {
	case lifetimeMessageHello, lifetimeMessageStop, lifetimeMessageClose:
		if message.Sequence != 0 || message.PID != 0 {
			return task.ErrEvidenceFault
		}
	case lifetimeMessageBegin, lifetimeMessageFailed, lifetimeMessageFinish:
		if message.Sequence == 0 || message.PID != 0 {
			return task.ErrEvidenceFault
		}
	case lifetimeMessageStarted:
		if message.Sequence == 0 || message.PID <= 0 {
			return task.ErrEvidenceFault
		}
	default:
		return task.ErrEvidenceFault
	}
	return nil
}

func validateLifetimeReply(reply lifetimeReply, request lifetimeMessage) error {
	if reply.Version != lifetimeProtocolVersion || reply.Type != request.Type || reply.Sequence != request.Sequence {
		return task.ErrEvidenceFault
	}
	if reply.Accepted == (reply.Code != "") {
		return task.ErrEvidenceFault
	}
	return nil
}

func osClosedError() error {
	return errors.New("task lifetime control is closed")
}
