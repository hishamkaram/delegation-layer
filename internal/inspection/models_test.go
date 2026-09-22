package inspection

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"

	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type testModelExchange struct {
	received int
	fail     bool
}

func (e *testModelExchange) Start() ([][]byte, error) {
	return [][]byte{[]byte(`{"method":"initialize"}`)}, nil
}

func (e *testModelExchange) Accept(line []byte) ([][]byte, bool, error) {
	e.received++
	if e.fail {
		return nil, false, errors.New("sensitive native details")
	}
	if string(line) == "ready" {
		return [][]byte{[]byte(`{"method":"model/list"}`)}, false, nil
	}
	return nil, string(line) == "models", nil
}

func testModelDefinition(t *testing.T, outputLimit int64) provider.InspectionDefinition {
	t.Helper()
	info, err := provider.LocateCLI("sh")
	if err != nil {
		t.Fatal(err)
	}
	return provider.InspectionDefinition{
		Executable: info.Path, ExecutableSHA256: info.SHA256,
		Directory: "/workspace", Environment: []string{}, OutputLimit: outputLimit,
	}
}

func TestModelExchangeOnlySendsMetadataRequests(t *testing.T) {
	var input bytes.Buffer
	exchange := &testModelExchange{}
	if err := runModelExchange(exchange, strings.NewReader("ready\nmodels\nignored\n"), &input); err != nil {
		t.Fatal(err)
	}
	if exchange.received != 2 || input.String() != "{\"method\":\"initialize\"}\n{\"method\":\"model/list\"}\n" {
		t.Fatalf("unexpected exchange: %d %q", exchange.received, input.String())
	}
}

func TestModelExchangeSanitizesProtocolFailure(t *testing.T) {
	err := runModelExchange(&testModelExchange{fail: true}, strings.NewReader("secret\n"), io.Discard)
	if !errors.Is(err, errNativeInspection) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestModelProjectionContainsPanics(t *testing.T) {
	definition := &provider.ModelDiscoveryDefinition{Project: func([]byte, []byte, []byte, bool) (provider.ModelCatalog, error) { panic("secret") }}
	if _, err := projectModels(definition, modelCommandResult{}, nil); !errors.Is(err, errNativeInspection) {
		t.Fatalf("panic escaped: %v", err)
	}
}

func TestDiscoveryFactsDoNotWidenHistoricalProjectionLimit(t *testing.T) {
	data := []byte(`{"models":"` + strings.Repeat("a", maxProjectedFacts) + `"}`)
	if _, err := canonicalProjectionFacts(data); err == nil {
		t.Fatal("historical limit widened")
	}
	op := &Operation{request: RequestRecord{Binding: Binding{DefinitionRevision: provider.ModelsRevision}}}
	if _, err := canonicalFactsWithin(data, op.factsLimit()); err != nil {
		t.Fatal(err)
	}
	op.request.Binding.DefinitionRevision = "legacy"
	if op.factsLimit() != maxProjectedFacts {
		t.Fatal("legacy journal limit changed")
	}
	if _, err := canonicalFactsWithin([]byte(strings.Repeat("a", task.MaxControlRecordSize)), task.MaxControlRecordSize/2); err == nil {
		t.Fatal("oversized model facts accepted")
	}
}

func TestModelCaptureRetainsFailureForSanitizedProjection(t *testing.T) {
	var captured modelCommandResult
	var starts int
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		definition := testModelDefinition(t, 128)
		hooks := nativeHooks{
			start: func(cmd *exec.Cmd) error {
				starts++
				if cmd.Stdin != nil {
					t.Error("listing got an input prompt")
				}
				if _, err := io.WriteString(cmd.Stdout, "models unavailable\n"); err != nil {
					return err
				}
				_, err := io.WriteString(cmd.Stderr, "authentication required\n")
				return err
			},
			wait: func(*exec.Cmd) error { return errors.New("native exit") },
		}
		var err error
		captured, err = runModelCommand(scope, definition, []string{"models"}, nil, hooks)
		return err
	})
	if workErr != nil || stopErr != nil || starts != 1 || captured.success || string(captured.stderr) != "authentication required\n" {
		t.Fatalf("capture lost native failure: %+v %v %v starts=%d", captured, workErr, stopErr, starts)
	}
}

func TestModelCaptureStartFailureJoinsStreams(t *testing.T) {
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		_, err := runModelCommand(scope, testModelDefinition(t, 64), nil, nil, nativeHooks{start: func(*exec.Cmd) error { return errors.New("secret start failure") }})
		return err
	})
	if !errors.Is(workErr, errNativeInspection) || stopErr != nil {
		t.Fatalf("unexpected result %v %v", workErr, stopErr)
	}
}

func TestModelCommandUsesVerifiedExecutableAfterPathReplacement(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "provider")
	replacement := filepath.Join(root, "replacement")
	for path, output := range map[string]string{original: "original", replacement: "replacement"} {
		script := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	info, err := provider.LocateCLIPath(original)
	if err != nil {
		t.Fatal(err)
	}
	definition := provider.InspectionDefinition{
		Executable: info.Path, ExecutableSHA256: info.SHA256,
		Directory: root, Environment: []string{}, OutputLimit: 128,
	}
	var captured modelCommandResult
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		captured, err = runModelCommand(scope, definition, nil, nil, nativeHooks{
			start: func(cmd *exec.Cmd) error {
				if renameErr := os.Rename(replacement, original); renameErr != nil {
					return renameErr
				}
				return cmd.Start()
			},
			wait: func(cmd *exec.Cmd) error { return cmd.Wait() },
		})
		return err
	})
	if workErr != nil || stopErr != nil || !captured.success || string(captured.stdout) != "original\n" {
		t.Fatalf("verified launch was not bound to the admitted file: result=%+v work=%v stop=%v", captured, workErr, stopErr)
	}
}

func TestModelCommandPreservesVerifiedScriptPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "provider-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$0\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	definition := runtimeDefinition(t, root, path, nil)
	var captured modelCommandResult
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		var err error
		captured, err = runModelCommand(scope, definition, nil, nil, nativeHooks{start: func(cmd *exec.Cmd) error { return cmd.Start() }, wait: func(cmd *exec.Cmd) error { return cmd.Wait() }})
		return err
	})
	if workErr != nil || stopErr != nil || !captured.success || string(captured.stdout) != definition.Executable+"\n" {
		t.Fatalf("verified script path was not preserved: result=%+v work=%v stop=%v", captured, workErr, stopErr)
	}
}

func TestModelCommandPreservesVerifiedNodeScriptPath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	root := t.TempDir()
	path := filepath.Join(root, "provider-cli")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env node\nconsole.log(process.argv[1])\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	definition := runtimeDefinition(t, root, path, nil)
	var captured modelCommandResult
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		var err error
		captured, err = runModelCommand(scope, definition, nil, nil, nativeHooks{start: func(cmd *exec.Cmd) error { return cmd.Start() }, wait: func(cmd *exec.Cmd) error { return cmd.Wait() }})
		return err
	})
	if workErr != nil || stopErr != nil || !captured.success || string(captured.stdout) != definition.Executable+"\n" {
		t.Fatalf("verified Node script path was not preserved: node=%s result=%+v work=%v stop=%v", node, captured, workErr, stopErr)
	}
}

func TestModelCatalogSurvivesInspectionJournalAndProof(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.DefinitionRevision = provider.ModelsRevision
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	catalog := provider.ModelCatalog{Status: "available", ReasonCode: "ok", Complete: true, Source: "test"}
	for i := range 100 {
		catalog.Models = append(catalog.Models, provider.ModelInfo{ID: fmt.Sprintf("model-%d", i), Name: strings.Repeat("n", 40)})
	}
	data, err := task.MarshalCanonical(catalog)
	mustInspection(t, err)
	if len(data) <= maxProjectedFacts {
		t.Fatal("fixture does not exceed historical limit")
	}
	permit, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, permit.Consume())
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.Complete(ResultEligible, data, time.Unix(102, 0)))
	if _, err = op.ReadProof(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing supervisor observation accepted")
	}
	mustInspection(t, op.RecordWorkerSuccess(42))
	reloaded, err := LoadOperation(store, request.TaskID)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, reloaded.Close()) })
	facts, err := reloaded.ReadProof()
	mustInspection(t, err)
	expected, err := canonicalFactsWithin(data, provider.MaxModelFactsBytes)
	mustInspection(t, err)
	if !bytes.Equal(facts, expected) {
		t.Fatal("model catalog changed across journal recovery")
	}
}

func TestModelDiscoveryDeadlinePreservesOrdinaryAdmission(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	for _, revision := range []string{provider.ModelsRevision, "legacy"} {
		binding := Binding{DefinitionRevision: revision}
		want := AdmissionTimeout
		if revision == provider.ModelsRevision {
			want = ModelDiscoveryTimeout
		}
		if got := TimeoutForRevision(revision); got != want {
			t.Fatalf("revision %q timeout=%s want=%s", revision, got, want)
		}
		record := RequestRecord{Binding: binding, CreatedAt: created.Format(time.RFC3339Nano), Deadline: created.Add(want).Format(time.RFC3339Nano)}
		if _, err := validateRequestTiming(record); err != nil {
			t.Fatal(err)
		}
		if revision == provider.ModelsRevision {
			record.Deadline = created.Add(time.Minute).Format(time.RFC3339Nano)
			if _, err := validateRequestTiming(record); err != nil {
				t.Fatalf("legacy model discovery deadline rejected: %v", err)
			}
		}
		record.Deadline = created.Add(want + time.Second).Format(time.RFC3339Nano)
		if _, err := validateRequestTiming(record); err == nil {
			t.Fatal("accepted modified deadline")
		}
	}
}

func TestModelDiscoveryVerifiesCapabilityBeforeListing(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "listed")
	path := filepath.Join(directory, "provider-cli")
	script := "#!/bin/sh\nset -eu\ncase \"${1-}\" in\n  --version) printf 'provider 1.0\\n' ;;\n  --help) printf 'usage without discovery flag\\n' ;;\n  --list-models) printf listed > \"" + marker + "\" ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	definition := runtimeDefinition(t, directory, path, nil)
	definition.Runtime = nil
	definition.Revision = provider.ModelsRevision
	definition.Models = &provider.ModelDiscoveryDefinition{
		Arguments:  []string{"--list-models"},
		Source:     "fixture",
		Capability: provider.RuntimeCapability{RequiredFlags: []string{"--list-models"}},
		Project: func([]byte, []byte, []byte, bool) (provider.ModelCatalog, error) {
			t.Fatal("discovery command ran after capability rejection")
			return provider.ModelCatalog{}, nil
		},
	}
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		_, err := inspectModels(scope, definition)
		return err
	})
	if workErr == nil || stopErr != nil {
		t.Fatalf("unsupported discovery capability was accepted: work=%v stop=%v", workErr, stopErr)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery command ran before capability rejection: %v", err)
	}
}

func TestModelDiscoveryRejectsExecutableReplacementBetweenCommands(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "provider-cli")
	script := "#!/bin/sh\nset -eu\ncase \"${1-}\" in\n  --version) printf 'provider 1.0\\n' ;;\n  --help) printf '\\n# changed\\n' >> \"$0\"; printf 'help\\n' ;;\n  *) printf executed > unexpected-model-launch ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	definition := runtimeDefinition(t, directory, path, nil)
	definition.Runtime = nil
	definition.Models = &provider.ModelDiscoveryDefinition{Arguments: []string{"models"}, Source: "fixture", Project: func([]byte, []byte, []byte, bool) (provider.ModelCatalog, error) {
		t.Error("replaced executable facts reached projection")
		return provider.ModelCatalog{}, nil
	}}
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}}, func(scope execution.PreflightScope) error {
		_, err := inspectModels(scope, definition)
		return err
	})
	if workErr == nil || stopErr != nil {
		t.Fatalf("expected identity rejection, got %v %v", workErr, stopErr)
	}
	if _, err := os.Stat(filepath.Join(directory, "unexpected-model-launch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement was executed: %v", err)
	}
}
