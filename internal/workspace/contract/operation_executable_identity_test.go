package contract

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func installedOperationRequestBytes(t *testing.T) []byte {
	t.Helper()
	data, err := task.MarshalCanonical(operationFixture(OperationCapture))
	if err != nil {
		t.Fatal(err)
	}
	legacy := `"worker_sha256":"` + repeatHex('6', 64) + `"`
	return bytes.Replace(data, []byte(legacy), []byte(`"executable_identity":"installed-path-v1"`), 1)
}

func TestOperationInstalledExecutableIdentityRoundTrip(t *testing.T) {
	data := installedOperationRequestBytes(t)
	var request OperationRequest
	if err := task.DecodeStrict(data, &request); err != nil {
		t.Fatalf("installed-path operation rejected: %v", err)
	}
	if _, err := ValidateOperation(request, request.RootID); err != nil {
		t.Fatalf("installed operation still requires a program digest: %v", err)
	}
	again, err := task.MarshalCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) || request.WorkerSHA256 != "" {
		t.Fatalf("installed request encoding changed: %s", again)
	}
	withEmpty := bytes.Replace(data, []byte(`"executable_identity"`), []byte(`"worker_sha256":"","executable_identity"`), 1)
	if err = task.DecodeStrict(withEmpty, &request); err != nil {
		t.Fatal(err)
	}
	if _, err = ValidateOperation(request, request.RootID); err != nil {
		t.Fatal(err)
	}
	again, err = task.MarshalCanonical(request)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("explicit empty digest changed canonical installed request: %v", err)
	}
}

func TestOperationExecutableIdentityRejectsUnknownMixedAndTruncatedLegacy(t *testing.T) {
	marked := installedOperationRequestBytes(t)
	legacy, err := task.MarshalCanonical(operationFixture(OperationCapture))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"unknown":            bytes.Replace(marked, []byte("installed-path-v1"), []byte("unknown"), 1),
		"mixed":              bytes.Replace(marked, []byte(`"executable_identity"`), []byte(`"worker_sha256":"`+strings.Repeat("a", 64)+`","executable_identity"`), 1),
		"truncated legacy":   bytes.Replace(legacy, []byte(`"worker_sha256":"`+repeatHex('6', 64)+`",`), nil, 1),
		"null discriminator": bytes.Replace(marked, []byte(`"installed-path-v1"`), []byte("null"), 1),
		"null digest":        bytes.Replace(marked, []byte(`"executable_identity"`), []byte(`"worker_sha256":null,"executable_identity"`), 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var request OperationRequest
			decodeErr := task.DecodeStrict(data, &request)
			if decodeErr == nil {
				_, decodeErr = ValidateOperation(request, request.RootID)
			}
			if decodeErr == nil {
				t.Fatal("invalid executable identity accepted")
			}
		})
	}
}
