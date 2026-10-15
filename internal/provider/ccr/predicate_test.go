package ccr

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestCredentialFailureUsesSealedExitContract(t *testing.T) {
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			interpreter, err := newCredentialInterpreter(Reference(mode), modeledReference(mode))
			if err != nil {
				t.Fatal(err)
			}
			input, raw := credentialInput(t, interpreter.Reference(), 77, "", "private native diagnostic")
			var answer bytes.Buffer
			result, err := interpreter.Evaluate(input, raw, &answer)
			if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalAuthenticationUnavailable {
				t.Fatalf("credential failure = %+v, %v", result, err)
			}
			if result.Session != nil || answer.Len() != 0 || len(result.Diagnostics) != 1 ||
				result.Diagnostics[0].Code != refusalAuthenticationUnavailable ||
				strings.Contains(result.Diagnostics[0].Message, "private") {
				t.Fatalf("credential failure exposed a session, answer, or native diagnostic: %+v", result)
			}
			if err := task.ValidateInterpretation(result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCredentialFailureDoesNotMaskOtherFailures(t *testing.T) {
	interpreter, err := newCredentialInterpreter(Reference(config.ModeReadOnly), modeledReference(config.ModeReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		code   int
		stdout string
		state  string
		error  string
	}{
		{name: "ordinary failure with auth text", code: 1},
		{name: "conflicting stdout", code: 77, stdout: "{}\n"},
		{name: "whitespace stdout", code: 77, stdout: "\n"},
		{name: "start failure", code: 77, state: task.InvocationStartFailed, error: "start failed"},
		{name: "seal failure", code: 77, error: "capture incomplete"},
		{name: "empty success", code: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, raw := credentialInput(t, interpreter.Reference(), test.code, test.stdout, "authentication failed")
			input.Seal.Error = test.error
			if test.state != "" {
				input.Seal.InvocationState = test.state
			}
			var answer bytes.Buffer
			result, err := interpreter.Evaluate(input, raw, &answer)
			if err != nil || result.Verdict != task.VerdictRejected || result.Refusal == refusalAuthenticationUnavailable || answer.Len() != 0 {
				t.Fatalf("other failure = %+v, %v", result, err)
			}
		})
	}
}

func TestCredentialInterpreterPreservesSuccessfulClaudeStream(t *testing.T) {
	interpreter, err := newCredentialInterpreter(Reference(config.ModeReadOnly), modeledReference(config.ModeReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	session := "123e4567-e89b-12d3-a456-426614174000"
	stdout := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":"fixture","apiKeySource":"none","cwd":"/workspace","tools":["Read","Glob","Grep"],"mcp_servers":[],"model":"fixture","permissionMode":"plan"}`+"\n"+
		`{"type":"result","subtype":"success","is_error":false,"result":"answer","stop_reason":"end_turn","session_id":%q,"duration_ms":1,"num_turns":1}`, session, session)
	input, raw := credentialInput(t, interpreter.Reference(), 0, stdout, "")
	input.ExpectedSession = task.SessionExpectation{Required: true, ID: session}
	var answer bytes.Buffer
	result, err := interpreter.Evaluate(input, raw, &answer)
	if err != nil || result.Verdict != task.VerdictCommitted || answer.String() != "answer" ||
		result.Session == nil || result.Session.Provider != Provider || result.Session.ConversationID != session {
		t.Fatalf("success was not preserved: %+v, answer=%q, error=%v", result, answer.String(), err)
	}
}

func TestCredentialFailurePreservesHistoricalInterpretationAndRecovery(t *testing.T) {
	registration := Registration()
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	old := preCredentialReference(config.ModeWorkspaceWrite, true)
	if old.SHA256 != "570f437fbc325941c7ea62d989d74f1c059b05206923978b7d5fb4ed0378e704" {
		t.Fatalf("published v3 contract changed: %+v", old)
	}
	interpreter, err := registry.Resolve(old)
	if err != nil {
		t.Fatal(err)
	}
	input, raw := credentialInput(t, old, 77, "", "credential unavailable")
	var answer bytes.Buffer
	previous, err := interpreter.Evaluate(input, raw, &answer)
	if err != nil || previous.Verdict != task.VerdictRejected || previous.Refusal == refusalAuthenticationUnavailable {
		t.Fatalf("historical interpretation changed: %+v, %v", previous, err)
	}
	recovery, err := registry.ResolveRecovery(old)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovery.Recover(input, raw, &answer)
	if err != nil || result.Refusal != refusalAuthenticationUnavailable || !input.Seal.Predicate.Equal(old) {
		t.Fatalf("credential recovery = %+v, %v", result, err)
	}
}

func TestCredentialFailureKeepsEvidenceAndIdentityFaultsOperational(t *testing.T) {
	interpreter, err := newCredentialInterpreter(Reference(config.ModeReadOnly), modeledReference(config.ModeReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	input, raw := credentialInput(t, interpreter.Reference(), 77, "", "")
	readError := errors.New("sealed evidence read failed")
	raw.readError = readError
	if _, err := interpreter.Evaluate(input, raw, io.Discard); !errors.Is(err, readError) {
		t.Fatalf("read fault became a credential result: %v", err)
	}
	raw.readError = nil
	input.Seal.Predicate = previousReference(config.ModeReadOnly)
	if _, err := interpreter.Evaluate(input, raw, io.Discard); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("predicate identity fault became a credential result: %v", err)
	}
}

func credentialInput(t *testing.T, reference task.PredicateRef, code int, stdout, stderr string) (predicate.Input, *credentialEvidence) {
	t.Helper()
	manifest := []task.RawManifestEntry{
		{Path: "raw/stderr", Size: int64(len(stderr)), SHA256: task.ComputeSHA256([]byte(stderr))},
		{Path: "raw/stdout", Size: int64(len(stdout)), SHA256: task.ComputeSHA256([]byte(stdout))},
	}
	data, err := task.MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	seal := task.ProviderExitRecord{
		SchemaVersion: task.SchemaVersion, RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32),
		SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64),
		InvocationState: task.InvocationStarted, ExitCode: code, Predicate: reference,
		ProviderExecutable: "/fixture/ccr", ProviderVersion: "fixture", ProviderSHA256: task.ComputeSHA256([]byte("fixture CCR")),
		RawManifest: manifest, ManifestSHA256: task.ComputeSHA256(data), ClosedAt: "2026-10-15T00:00:00Z",
	}
	return predicate.Input{Seal: seal}, &credentialEvidence{stdout: stdout, stderr: stderr}
}

type credentialEvidence struct {
	stdout    string
	stderr    string
	readError error
}

func (e *credentialEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	if e.readError != nil {
		return e.readError
	}
	switch stream {
	case predicate.Stdout:
		return consume(strings.NewReader(e.stdout))
	case predicate.Stderr:
		return consume(strings.NewReader(e.stderr))
	default:
		return fmt.Errorf("unexpected evidence stream %d", stream)
	}
}
