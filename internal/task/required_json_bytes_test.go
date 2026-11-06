package task

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestDecodeStrictByteSliceCanonicalRoundTrip(t *testing.T) {
	record := struct {
		Data []byte `json:"data"`
	}{Data: []byte{0, 1, 255}}
	encoded, err := MarshalCanonical(record)
	if err != nil || string(encoded) != "{\"data\":\"AAH/\"}\n" {
		t.Fatalf("canonical byte representation: %q %v", encoded, err)
	}
	for _, input := range []string{string(encoded), "{\"data\": \n \"AAH/\" \t}", `{"data":[0,1,255]}`} {
		var restored struct {
			Data []byte `json:"data"`
		}
		if err = DecodeStrict([]byte(input), &restored); err != nil || !bytes.Equal(restored.Data, record.Data) {
			t.Fatalf("valid native byte representation refused: %v", err)
		}
	}
	var empty struct {
		Data []byte `json:"data"`
	}
	if err = DecodeStrict([]byte(`{"data":""}`), &empty); err != nil || empty.Data == nil || len(empty.Data) != 0 {
		t.Fatalf("explicit empty bytes changed: %v", err)
	}
}

func TestDecodeStrictByteSlicePreservesStructuralChecks(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"data":null}`, `{"data":"not base64!"}`, `{"data":"AAH/","other":true}`,
		`{"data":"AAH/","data":"AAH/"}`, `{"data":"AAH/","Data":"AAH/"}`,
		`{"data":"AAH/"} {}`, `{"data":[256]}`,
	} {
		var record struct {
			Data []byte `json:"data"`
		}
		if err := DecodeStrict([]byte(input), &record); err == nil {
			t.Fatal("invalid byte record accepted")
		}
	}
	var record struct {
		Data []byte `json:"data"`
	}
	input := []byte(`{"data":"` + strings.Repeat("A", MaxControlRecordSize) + `"}`)
	if err := DecodeStrict(input, &record); !errors.Is(err, ErrControlRecordTooBig) {
		t.Fatalf("byte field bypassed control-record limit: %v", err)
	}
}

func TestDecodeStrictByteSliceSpecialCaseStaysNarrow(t *testing.T) {
	var array struct {
		Data [3]byte `json:"data"`
	}
	if err := DecodeStrict([]byte(`{"data":"AAH/"}`), &array); err == nil {
		t.Fatal("base64 accepted for a fixed-size byte array")
	}
	var stringsRecord struct {
		Data []string `json:"data"`
	}
	if err := DecodeStrict([]byte(`{"data":"AAH/"}`), &stringsRecord); err == nil {
		t.Fatal("base64 accepted for a non-byte slice")
	}
}
