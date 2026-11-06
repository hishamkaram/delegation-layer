package lifetime

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Native fixture receipts and asynchronous handshakes share one publication
// owner. Readers see a complete file; create-once handshakes preserve winners.
func publishNativeFixtureRecord(path string, replace bool, encode func(io.Writer) error) error {
	if !filepath.IsAbs(path) {
		return errors.New("native fixture record path is not absolute")
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := encode(file)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	if replace {
		if err = os.Rename(temporary, path); err != nil {
			return errors.Join(err, os.Remove(temporary))
		}
		return nil
	}
	if err = os.Link(temporary, path); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	return os.Remove(temporary)
}

func TestNativeFixtureRecordDoesNotExposePartialEncoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	partial, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- publishNativeFixtureRecord(path, false, func(writer io.Writer) error {
			_, err := io.WriteString(writer, `{"pid":`)
			close(partial)
			<-finish
			if err == nil {
				_, err = io.WriteString(writer, "123}\n")
			}
			return err
		})
	}()
	select {
	case <-partial:
	case err := <-done:
		t.Fatalf("publisher exited before partial encoding: %v", err)
	case <-time.After(5 * time.Second):
		close(finish)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("publisher did not exit after release")
		}
		t.Fatal("publisher did not enter encoding")
	}
	_, inspectErr := os.Stat(path)
	close(finish)
	writeErr := <-done
	if !errors.Is(inspectErr, os.ErrNotExist) || writeErr != nil {
		t.Fatalf("partial publication: stat=%v write=%v", inspectErr, writeErr)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{\"pid\":123}\n" {
		t.Fatalf("complete record=%q err=%v", data, err)
	}
	err = publishNativeFixtureRecord(path, false, func(writer io.Writer) error {
		_, writeErr := io.WriteString(writer, "replacement")
		return writeErr
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("create-once replacement error=%v", err)
	}
}
