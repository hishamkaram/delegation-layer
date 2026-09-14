package predicate

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type testInterpreter struct{ ref task.PredicateRef }

func (i testInterpreter) Reference() task.PredicateRef { return i.ref }
func (testInterpreter) Evaluate(Input, Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{}, nil
}

func TestRegistryUsesFullFrozenReference(t *testing.T) {
	ref := task.FixturePredicateRef()
	r, err := New(FixtureV1())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Resolve(ref); err != nil {
		t.Fatal(err)
	}
	cases := []task.PredicateRef{ref, ref, ref, ref}
	cases[0].Adapter = "codex"
	cases[1].Mode = "workspace-write"
	cases[2].Version = "2"
	cases[3].SHA256 = task.ComputeSHA256([]byte("other"))
	for _, candidate := range cases {
		if _, err = r.Resolve(candidate); !errors.Is(err, task.ErrIncompatiblePredicate) {
			t.Fatalf("accepted mismatched reference: %+v %v", candidate, err)
		}
	}
	if _, err = New(FixtureV1(), FixtureV1()); err == nil {
		t.Fatal("duplicate registry reference")
	}
	if _, err = New(nil); err == nil {
		t.Fatal("nil interpreter accepted")
	}
	var typedNil *testInterpreter
	if _, err = New(typedNil); err == nil {
		t.Fatal("typed-nil interpreter accepted")
	}
	empty, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = empty.Resolve(ref); !errors.Is(err, task.ErrIncompatiblePredicate) {
		t.Fatal(err)
	}
}

type rawEvidence map[Stream]string

func (r rawEvidence) Read(s Stream, consume func(io.Reader) error) error {
	return consume(bytes.NewBufferString(r[s]))
}

func TestFixtureV1BridgePreservesContract(t *testing.T) {
	ref := task.FixturePredicateRef()
	for _, content := range []string{"hello\n", " \t\n", ""} {
		raw := []task.RawManifestEntry{{Path: "raw/stderr", SHA256: task.ComputeSHA256(nil)}, {Path: "raw/stdout", Size: int64(len(content)), SHA256: task.ComputeSHA256([]byte(content))}}
		data, err := task.MarshalCanonical(raw)
		if err != nil {
			t.Fatal(err)
		}
		seal := task.ProviderExitRecord{SchemaVersion: task.SchemaVersion, RootID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TaskID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", SpecSHA256: task.ComputeSHA256([]byte("spec")), MetaSHA256: task.ComputeSHA256([]byte("meta")), InvocationState: task.InvocationStarted, Predicate: ref, RawManifest: raw, ManifestSHA256: task.ComputeSHA256(data), ClosedAt: "2026-09-13T00:00:00Z"}
		want, err := task.EvaluateRegisteredPredicate(ref, &seal)
		if err != nil {
			t.Fatal(err)
		}
		var answer bytes.Buffer
		got, err := FixtureV1().Evaluate(Input{Seal: seal}, rawEvidence{Stdout: content}, &answer)
		if err != nil {
			t.Fatal(err)
		}
		if got.Verdict != want.Verdict || got.Refusal != string(want.PayloadContent) {
			t.Fatalf("v1 decision changed: %+v %+v", got, want)
		}
		if want.RawPath != "" && answer.String() != content {
			t.Fatal("v1 answer bytes changed")
		}
	}
}

func TestFixtureV1BridgePreservesRefusalPrecedence(t *testing.T) {
	tests := []struct {
		name, invocationState, errorText string
		exitCode                         int
		stdout                           string
		want                             string
	}{
		{name: "start failure wins", invocationState: task.InvocationStartFailed, exitCode: 7, errorText: "start failed", stdout: "answer", want: "start_failed: start failed"},
		{name: "capture error wins", invocationState: task.InvocationStarted, exitCode: 0, errorText: "capture failed", stdout: "answer", want: "capture_error: capture failed"},
		{name: "nonzero exit", invocationState: task.InvocationStarted, exitCode: 7, stdout: "answer", want: "provider_exit: 7"},
		{name: "empty answer", invocationState: task.InvocationStarted, exitCode: 0, stdout: "", want: "empty_answer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref := task.FixturePredicateRef()
			raw := []task.RawManifestEntry{{Path: "raw/stderr", SHA256: task.ComputeSHA256(nil)}, {Path: "raw/stdout", Size: int64(len(test.stdout)), SHA256: task.ComputeSHA256([]byte(test.stdout))}}
			data, err := task.MarshalCanonical(raw)
			if err != nil {
				t.Fatal(err)
			}
			seal := task.ProviderExitRecord{SchemaVersion: task.SchemaVersion, RootID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TaskID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", SpecSHA256: task.ComputeSHA256([]byte("spec")), MetaSHA256: task.ComputeSHA256([]byte("meta")), InvocationState: test.invocationState, ExitCode: test.exitCode, Error: test.errorText, Predicate: ref, RawManifest: raw, ManifestSHA256: task.ComputeSHA256(data), ClosedAt: "2026-09-13T00:00:00Z"}
			var answer bytes.Buffer
			got, err := FixtureV1().Evaluate(Input{Seal: seal}, rawEvidence{Stdout: test.stdout}, &answer)
			if err != nil {
				t.Fatal(err)
			}
			if got.Verdict != task.VerdictRejected || got.Refusal != test.want || answer.Len() != 0 {
				t.Fatalf("bridge changed refusal precedence: %+v answer=%q", got, answer.String())
			}
		})
	}
}
