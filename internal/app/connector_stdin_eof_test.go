package app

import (
	"errors"
	"io"
	"os"
	"testing"
)

func TestConnectorJSONAcceptsPipeEOF(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := input.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := writer.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Error(closeErr)
		}
	})
	if _, err := io.WriteString(writer, `{"value":"pipe"}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Value string `json:"value"`
	}
	if err := readConnectorJSON(t.Context(), "-", input, &decoded); err != nil {
		t.Fatalf("valid JSON ending at pipe EOF was rejected: %v", err)
	}
	if decoded.Value != "pipe" {
		t.Fatalf("decoded value = %q, want %q", decoded.Value, "pipe")
	}
	if _, err := input.Stat(); err != nil {
		t.Fatalf("command closed borrowed input: %v", err)
	}
}
