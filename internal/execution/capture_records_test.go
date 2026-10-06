package execution

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

// No provider process is needed: the ordinary capture pipe exercises framing,
// asynchronous reader lifetime, and the final unterminated record directly.
func TestCaptureRecordViewsRemainReadableUntilObservationCompletes(t *testing.T) {
	td, permit, _, _ := fixtureTask(t, "ordinary")
	defer func() { require(t, permit.Release()) }()
	require(t, permit.Consume())
	captured, err := newCapture(td, "stdout")
	require(t, err)
	observer := &retainingRecordObserver{}
	go captured.run(Options{Identity: observer})
	data := `{"opaque":"` + strings.Repeat("x", 2<<20) + `","type":"first"}` + "\n" + `{"type":"last"}`
	_, err = io.Copy(captured.writer, strings.NewReader(data))
	require(t, err)
	require(t, captured.writer.Close())
	require(t, <-captured.done)
	require(t, observer.Complete())
	if _, ok := observer.values[0].Reader.(io.Closer); ok {
		t.Fatal("record view exposes core file ownership")
	}
	if len(observer.types) != 2 || observer.types[0] != "first" || observer.types[1] != "last" {
		t.Fatalf("observed=%v", observer.types)
	}
	require(t, captured.closeObserver())
	if _, err = observer.values[0].Object("type"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("record reader retained access after close: %v", err)
	}
}

type retainingRecordObserver struct {
	values []providerjson.Value
	types  []string
}

func (*retainingRecordObserver) Observe([]byte) {}
func (o *retainingRecordObserver) ObserveRecord(value providerjson.Value) error {
	o.values = append(o.values, value)
	return nil
}

func (o *retainingRecordObserver) Complete() error {
	for _, value := range o.values {
		fields, err := value.Object("type")
		if err != nil {
			return err
		}
		name, err := fields["type"].String(32)
		if err != nil {
			return err
		}
		o.types = append(o.types, name)
	}
	return nil
}

func TestCapturedReaderIOFailureRemainsOperational(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw")
	require(t, os.WriteFile(path, []byte("x"), 0o600))
	file, err := os.Open(path)
	require(t, err)
	reader := &observedReader{file: file}
	var data [2]byte
	if _, err = reader.ReadAt(data[:], 0); !errors.Is(err, io.EOF) {
		t.Fatalf("truncated read=%v", err)
	}
	if err = reader.Close(); !errors.Is(err, io.EOF) {
		t.Fatalf("capture read failure was lost=%v", err)
	}
}
