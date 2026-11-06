//go:build linux || darwin

package lifetime

import (
	"bytes"
	"encoding/json"
	"testing"
)

// This is a JSON data fixture only; it creates no scope, process or executable.
func TestNativeProofInstalledBinaryJSONParity(t *testing.T) {
	fixture := struct {
		Binary nativeBinaryIdentity `json:"binary"`
	}{nativeBinaryIdentity{Name: "lifetime.test", Path: "/installed/café<&>/lifetime.test", SizeBytes: 1, GoVersion: "go1.27.3"}}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// Same independent byte fixture as the Python primitive data digest test.
	want := []byte(`{"binary":{"name":"lifetime.test","path":"/installed/café\u003c\u0026\u003e/lifetime.test","size_bytes":1,"go_version":"go1.27.3"}}`)
	if !bytes.Equal(encoded, want) {
		t.Fatalf("installed metadata JSON differs from the proof reader: %s", encoded)
	}
}
