package contract

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRequestRequiresExactlyOneProviderSelection(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		auto     bool
		valid    bool
	}{
		{name: "auto", auto: true, valid: true},
		{name: "explicit provider", provider: "claude:print", valid: true},
		{name: "auto and provider", provider: "claude:print", auto: true},
		{name: "neither", provider: ""},
		{name: "unstructured provider", provider: "claude"},
		{name: "uppercase provider", provider: "Claude:print"},
	}
	for _, testCase := range cases {
		request := Request{Provider: testCase.provider, Auto: testCase.auto, CanonicalCwd: "/work"}
		_, err := MarshalRequest(request)
		if (err == nil) != testCase.valid {
			t.Errorf("%s: err=%v, want valid=%v", testCase.name, err, testCase.valid)
		}
	}
}

func TestAutoRequestPreservesOmittedOptions(t *testing.T) {
	request := Request{Auto: true, CanonicalCwd: "/work"}
	encoded, err := MarshalRequest(request)
	if err != nil {
		t.Fatalf("auto request rejected: %v", err)
	}
	if want := `{"auto":true,"canonical_cwd":"/work","config":{}}` + "\n"; string(encoded) != want {
		t.Fatalf("encoded %q, want %q: no provider, mode, or budget may be chosen", encoded, want)
	}
	decoded, err := DecodeRequest(encoded)
	if err != nil || !reflect.DeepEqual(decoded, request) {
		t.Fatalf("decoded %+v err=%v, want %+v", decoded, err, request)
	}
}

func TestRequestRejectsNoncanonicalCwdAndInteractive(t *testing.T) {
	for _, cwd := range []string{"", " ", "work", "/work/", "/work/../other", "/work/./x", "/wo\x00rk"} {
		request := Request{Auto: true, CanonicalCwd: cwd}
		if _, err := MarshalRequest(request); err == nil {
			t.Errorf("cwd %q accepted", cwd)
		}
	}
	interactive := Request{Auto: true, CanonicalCwd: "/work", Config: task.TaskConfig{Interactive: true}}
	if _, err := MarshalRequest(interactive); err == nil {
		t.Fatal("interactive request accepted")
	}
}

func TestRequestEncodingIsBoundedToOneControlRecord(t *testing.T) {
	request := Request{Auto: true, CanonicalCwd: "/work", Config: task.TaskConfig{Model: strings.Repeat("m", task.MaxControlRecordSize)}}
	if _, err := MarshalRequest(request); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized request err=%v, want ErrControlRecordTooBig", err)
	}
}

func TestDecodeRequestAcceptsOnlyCanonicalStoredBytes(t *testing.T) {
	encoded, err := MarshalRequest(testRequest())
	if err != nil {
		t.Fatalf("request rejected: %v", err)
	}
	decoded, err := DecodeRequest(encoded)
	if err != nil || !reflect.DeepEqual(decoded, testRequest()) {
		t.Fatalf("round trip %+v err=%v", decoded, err)
	}
	refused := map[string]string{
		"reordered keys":     `{"canonical_cwd":"/work","auto":true,"config":{}}` + "\n",
		"missing newline":    `{"auto":true,"canonical_cwd":"/work","config":{}}`,
		"indented":           "{\n \"auto\": true,\n \"canonical_cwd\": \"/work\",\n \"config\": {}\n}\n",
		"unknown field":      `{"auto":true,"canonical_cwd":"/work","config":{},"budget_nanos":1}` + "\n",
		"missing config":     `{"auto":true,"canonical_cwd":"/work"}` + "\n",
		"duplicate key":      `{"auto":true,"auto":true,"canonical_cwd":"/work","config":{}}` + "\n",
		"invalid selection":  `{"canonical_cwd":"/work","config":{}}` + "\n",
		"prior session":      `{"auto":true,"canonical_cwd":"/work","config":{},"prior_session":{}}` + "\n",
		"interactive config": `{"auto":true,"canonical_cwd":"/work","config":{"interactive":true}}` + "\n",
	}
	for name, data := range refused {
		if _, err := DecodeRequest([]byte(data)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
