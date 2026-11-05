//go:build linux || darwin

package lifetime

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestAdmissionNativeCBORRejectsMalformedFrames(t *testing.T) {
	for name, data := range map[string][]byte{
		"indefinite":  {0xbf},
		"truncated":   {0xa1, 0x61, 'x', 0x1b, 0},
		"duplicate":   {0xa2, 0x61, 'x', 0, 0x61, 'x', 1},
		"trailing":    {0xa0, 0},
		"nontext key": {0xa1, 0, 0},
		"map limit":   {0xb1},
		"array limit": {0xa1, 0x61, 'x', 0x99, 0x10, 1},
		"nesting":     append(append([]byte{0xa1, 0x61, 'x'}, bytes.Repeat([]byte{0x81}, 14)...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := admissionCBORFields(data); err == nil {
				t.Fatal("malformed or unbounded native frame accepted")
			}
		})
	}
	// Pinned Rust Request::Enqueue: tagged enum maps, TaskIds([7]), no delay.
	expected, err := hex.DecodeString("a167456e7175657565a2657461736b73a1675461736b49647381076a656e71756575655f6174f6")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(admissionEnqueueRequest(), expected) {
		t.Fatal("single-ID Enqueue fixture differs from independent wire shape")
	}
}

// The following fixture codec implements only bounded definite-length values
// emitted by pinned Pueue4.0.4/ciborium. Rust is the independent wire oracle.
type admissionCBORCursor struct {
	data   []byte
	offset int
}

func (c *admissionCBORCursor) head() (byte, uint64, error) {
	if c.offset >= len(c.data) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	first := c.data[c.offset]
	c.offset++
	major, value := first>>5, uint64(first&31)
	if value < 24 {
		return major, value, nil
	}
	width := 0
	switch value {
	case 24:
		width = 1
	case 25:
		width = 2
	case 26:
		width = 4
	case 27:
		width = 8
	default:
		return 0, 0, errors.New("unsupported indefinite CBOR fixture value")
	}
	if len(c.data)-c.offset < width {
		return 0, 0, io.ErrUnexpectedEOF
	}
	value = 0
	for _, b := range c.data[c.offset : c.offset+width] {
		value = value<<8 | uint64(b)
	}
	c.offset += width
	return major, value, nil
}

func (c *admissionCBORCursor) skip(depth int) error {
	if depth > 12 {
		return errors.New("nested fixture CBOR limit")
	}
	major, size, err := c.head()
	if err != nil {
		return err
	}
	switch major {
	case 0, 1:
		return nil
	case 2, 3:
		if size > uint64(len(c.data)-c.offset) {
			return io.ErrUnexpectedEOF
		}
		c.offset += int(size) // Checked against the bounded native frame above.
		return nil
	case 4, 5:
		return c.skipCollection(major, size, depth)
	case 7:
		if size == 20 || size == 21 || size == 22 {
			return nil
		}
		return errors.New("unsupported fixture CBOR simple value")
	default:
		return errors.New("unsupported fixture CBOR type")
	}
}

func (c *admissionCBORCursor) skipCollection(major byte, size uint64, depth int) error {
	if size > 4096 {
		return errors.New("fixture CBOR collection limit")
	}
	for range size {
		if err := c.skip(depth + 1); err != nil {
			return err
		}
		if major == 5 {
			if err := c.skip(depth + 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func admissionCBORString(data []byte) (string, error) {
	cursor := admissionCBORCursor{data: data}
	major, size, err := cursor.head()
	if err != nil {
		return "", err
	}
	if major != 3 || size != uint64(len(data)-cursor.offset) {
		return "", errors.New("fixture CBOR text expected")
	}
	return string(data[cursor.offset:]), nil
}

func admissionCBORFields(data []byte) (map[string][]byte, error) {
	cursor := admissionCBORCursor{data: data}
	major, size, err := cursor.head()
	if err != nil {
		return nil, err
	}
	if major != 5 || size > 16 {
		return nil, errors.New("bounded fixture CBOR map expected")
	}
	fields := make(map[string][]byte)
	for range size {
		start := cursor.offset
		if err = cursor.skip(0); err != nil {
			return nil, err
		}
		key, keyErr := admissionCBORString(data[start:cursor.offset])
		if keyErr != nil {
			return nil, keyErr
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate fixture CBOR key")
		}
		start = cursor.offset
		if err = cursor.skip(0); err != nil {
			return nil, err
		}
		fields[key] = data[start:cursor.offset]
	}
	if cursor.offset != len(data) {
		return nil, errors.New("trailing fixture CBOR")
	}
	return fields, nil
}

func (p *admissionNativeProof) decodeAdd(data []byte) (admissionNativeAdd, error) {
	outer, err := admissionCBORFields(data)
	if err != nil {
		return admissionNativeAdd{}, err
	}
	if len(outer) != 1 {
		return admissionNativeAdd{}, errors.New("native Add variant expected")
	}
	fields, err := admissionCBORFields(outer["Add"])
	if err != nil {
		return admissionNativeAdd{}, err
	}
	if !bytes.Equal(fields["stashed"], []byte{0xf5}) || !bytes.Equal(fields["start_immediately"], []byte{0xf4}) {
		return admissionNativeAdd{}, errors.New("native request can bypass Stashed ordering")
	}
	var add admissionNativeAdd
	for key, target := range map[string]*string{"command": &add.command, "path": &add.path, "group": &add.group, "label": &add.label} {
		*target, err = admissionCBORString(fields[key])
		if err != nil {
			return add, err
		}
	}
	if add.label != p.subject.Label || add.group != p.subject.Group || !strings.Contains(add.command, p.subject.TaskID) || !strings.Contains(add.command, "--root") {
		return add, errors.New("native Add does not match actual journal subject")
	}
	info, err := os.Stat(add.path)
	if err != nil {
		return add, err
	}
	if !os.SameFile(info, p.managed.root.info) {
		return add, errors.New("native Add cwd changed root")
	}
	return add, nil
}

type admissionCBORPair struct{ key, value []byte }

func admissionCBORHead(major byte, size uint64) []byte {
	if size < 24 {
		return []byte{major<<5 | byte(size)}
	}
	if size <= 255 {
		return []byte{major<<5 | 24, byte(size)}
	}
	if size <= 65535 {
		return []byte{major<<5 | 25, byte(size >> 8), byte(size)}
	}
	data := make([]byte, 9)
	data[0] = major<<5 | 27
	binary.BigEndian.PutUint64(data[1:], size)
	return data
}

func admissionCBORText(value string) []byte {
	return append(admissionCBORHead(3, uint64(len(value))), []byte(value)...)
}

func admissionCBORNamed(key string, value []byte) admissionCBORPair {
	return admissionCBORPair{key: admissionCBORText(key), value: value}
}

func admissionCBORMap(fields ...admissionCBORPair) []byte {
	data := admissionCBORHead(5, uint64(len(fields)))
	for _, field := range fields {
		data = append(data, field.key...)
		data = append(data, field.value...)
	}
	return data
}

func admissionAddedReply() []byte {
	return admissionCBORMap(admissionCBORNamed("AddedTask", admissionCBORMap(admissionCBORNamed("task_id", []byte{7}), admissionCBORNamed("enqueue_at", []byte{0xf6}), admissionCBORNamed("group_is_paused", []byte{0xf4}))))
}

func admissionSuccessReply() []byte {
	return admissionCBORMap(admissionCBORNamed("Success", admissionCBORText("complete")))
}

func admissionEnqueueRequest() []byte {
	return admissionCBORMap(admissionCBORNamed("Enqueue", admissionCBORMap(admissionCBORNamed("tasks", admissionCBORMap(admissionCBORNamed("TaskIds", []byte{0x81, 7}))), admissionCBORNamed("enqueue_at", []byte{0xf6}))))
}

func (p *admissionNativeProof) statusReply() []byte {
	parallel := byte(1)
	if p.subject.Group == "default" {
		parallel = 0
	}
	group := admissionCBORMap(admissionCBORNamed("status", admissionCBORText("Running")), admissionCBORNamed("parallel_tasks", []byte{parallel}))
	groups := admissionCBORMap(admissionCBORNamed(p.subject.Group, group))
	tasks := admissionCBORMap()
	if p.add.command != "" {
		row := admissionCBORMap(admissionCBORNamed("id", []byte{7}), admissionCBORNamed("created_at", admissionCBORText("2026-11-04T00:00:00Z")), admissionCBORNamed("original_command", admissionCBORText(p.add.command)), admissionCBORNamed("command", admissionCBORText(p.add.command)), admissionCBORNamed("path", admissionCBORText(p.add.path)), admissionCBORNamed("envs", admissionCBORMap()), admissionCBORNamed("group", admissionCBORText(p.add.group)), admissionCBORNamed("dependencies", []byte{0x80}), admissionCBORNamed("priority", []byte{0}), admissionCBORNamed("label", admissionCBORText(p.add.label)), admissionCBORNamed("status", admissionCBORMap(admissionCBORNamed("Stashed", admissionCBORMap(admissionCBORNamed("enqueue_at", []byte{0xf6}))))))
		tasks = admissionCBORMap(admissionCBORPair{key: []byte{7}, value: row})
	}
	return admissionCBORMap(admissionCBORNamed("Status", admissionCBORMap(admissionCBORNamed("tasks", tasks), admissionCBORNamed("groups", groups))))
}

func (p *admissionNativeProof) repairEditableTask(command string) []byte {
	return admissionCBORMap(
		admissionCBORNamed("id", []byte{7}), admissionCBORNamed("command", admissionCBORText(command)),
		admissionCBORNamed("path", admissionCBORText(p.store.Root)), admissionCBORNamed("label", []byte{0xf6}),
		admissionCBORNamed("priority", []byte{0}),
	)
}

func (p *admissionNativeProof) exchangeRepairEdit(ctx context.Context) error {
	connection := p.managed.proof.connection
	if err := writeShutdownLeaseFrame(connection, []byte("0.31.1")); err != nil {
		return err
	}
	frame, err := readShutdownLeaseFrame(connection)
	if err != nil {
		return fmt.Errorf("read native EditRequest: %w", err)
	}
	request := admissionCBORMap(admissionCBORNamed("EditRequest", []byte{0x81, 7}))
	if !bytes.Equal(frame, request) {
		return errors.New("native repair CLI did not send exact single-ID EditRequest")
	}
	command := "old command"
	expected := admissionCBORMap(admissionCBORNamed("EditedTasks", append([]byte{0x81}, p.repairEditableTask("replacement")...)))
	if strings.HasSuffix(p.role, "-restore") {
		command = "changed command"
		expected = admissionCBORMap(admissionCBORNamed("EditRestore", []byte{0x81, 7}))
	}
	reply := admissionCBORMap(admissionCBORNamed("Edit", append([]byte{0x81}, p.repairEditableTask(command)...)))
	if p.survivor != nil {
		p.survivor.launchExpected = true
	}
	if err = writeShutdownLeaseFrame(connection, reply); err != nil {
		return err
	}
	frame, err = readShutdownLeaseFrame(connection)
	if err != nil {
		return fmt.Errorf("read native edited/restore request: %w", err)
	}
	// Rust independently serializes the last frame after the actual Go editor
	// exits. Exact bytes also verify unchanged numeric identity and properties.
	if !bytes.Equal(frame, expected) {
		return errors.New("native repair final EditedTasks/EditRestore differs from required command and identity")
	}
	if p.survivor != nil {
		if err = p.survivor.capture(ctx, p.managed.proof.generation); err != nil {
			return err
		}
	}
	return nil
}

func (p *admissionNativeProof) finishRepair(ctx context.Context, t *testing.T) error {
	if err := p.exchangeRepairEdit(ctx); err != nil {
		return err
	}
	connection := p.managed.proof.connection
	// Keep the owner's stderr reader alive while Rust reports an edit failure;
	// otherwise a broken diagnostic pipe can end it before EditRestore. Lose
	// the owner at the final protocol gate, before its reply is delivered.
	if err := p.loseOwner(ctx); err != nil {
		return err
	}
	if err := p.managed.proof.inspect(ctx, true); err != nil {
		return err
	}
	if err := p.probe(ctx, "busy"); err != nil {
		return err
	}
	if err := writeShutdownLeaseFrame(connection, admissionSuccessReply()); err != nil {
		return err
	}
	if err := p.managed.proof.watches["cli"].wait(ctx); err != nil {
		return err
	}
	if err := WaitProcessAbsent(ctx, p.managed.proof.refs["cli"]); err != nil {
		return err
	}
	if err := p.probe(ctx, "acquired"); err != nil {
		return err
	}
	t.Log("real pinned Rust edit and managed Go editor: user shell bypassed; RootSH survives caller exit/owner death through last EditedTasks or EditRestore and reply, then fresh EX acquires")
	return nil
}
