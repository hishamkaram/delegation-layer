package task

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestStrictRawJSONRetainsOpaqueValueWithoutWeakeningEnvelope(t *testing.T) {
	type envelope struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	var decoded envelope
	if err := DecodeStrict([]byte(`{"name":"remote","payload":{"publication":"committed"}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Payload) != `{"publication":"committed"}` {
		t.Fatalf("raw payload changed: %s", decoded.Payload)
	}
	cases := []string{
		`{"payload":{"publication":"committed"}}`,
		`{"name":"remote","payload":null}`,
		`{"name":"remote","payload":{"state":1,"state":2}}`,
		`{"name":"remote","payload":{},"unknown":true}`,
	}
	for _, value := range cases {
		if err := DecodeStrict([]byte(value), &decoded); err == nil {
			t.Fatalf("strict envelope accepted %s", value)
		}
	}
	if err := DecodeStrict([]byte(`{"name":"remote","payload":{"state":1,"state":2}}`), &decoded); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("opaque value bypassed duplicate check: %v", err)
	}
}
