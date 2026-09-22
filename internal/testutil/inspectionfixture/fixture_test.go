package inspectionfixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type fixtureFiles struct {
	dir          string
	outerPath    string
	helperPath   string
	helperConfig string
	artifactDir  string
	sentinelPath string
	sentinel     []byte
	outer        Config
	helper       HelperConfig
}

func newFixtureFiles(t *testing.T) fixtureFiles {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(dir, "inspection-helper")
	writeFixtureFile(t, helperPath, []byte("fixture helper executable\n"), 0o700)
	artifactDir := filepath.Join(dir, "markers")
	if err = os.Mkdir(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("private-inspection-sentinel")
	sentinelPath := filepath.Join(dir, "sentinel")
	writeFixtureFile(t, sentinelPath, sentinel, 0o600)
	helperConfig := HelperConfig{SchemaVersion: SchemaVersion, ArtifactDir: artifactDir, SentinelPath: sentinelPath, Eligible: true}
	helperConfigPath := filepath.Join(dir, "helper.json")
	writeFixtureJSON(t, helperConfigPath, helperConfig)
	outer := Config{SchemaVersion: SchemaVersion, HelperExecutable: helperPath, HelperSHA256: hashFixtureFile(t, helperPath), HelperConfig: helperConfigPath, HelperConfigSHA256: hashFixtureFile(t, helperConfigPath), Environment: []string{"LANG=C", "FIXTURE=yes"}}
	outerPath := filepath.Join(dir, ConfigName)
	writeFixtureJSON(t, outerPath, outer)
	return fixtureFiles{dir: dir, outerPath: outerPath, helperPath: helperPath, helperConfig: helperConfigPath, artifactDir: artifactDir, sentinelPath: sentinelPath, sentinel: sentinel, outer: outer, helper: helperConfig}
}

func writeFixtureFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func writeFixtureJSON(t *testing.T, path string, value any) []byte {
	t.Helper()
	data, err := task.MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, data, 0o600)
	return data
}

func hashFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return task.ComputeSHA256(data)
}

func fixtureRequest(dir string) task.TaskRecord {
	return task.TaskRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          strings.Repeat("a", 32),
		TaskID:          strings.Repeat("b", 32),
		Provider:        "fixture:test",
		Mode:            "read-only",
		CanonicalCwd:    dir,
		RequestedConfig: task.TaskConfig{Permission: "read-only", Budget: "30s"},
		BudgetNanos:     int64(30 * time.Second),
		BriefSHA256:     task.ComputeSHA256([]byte("brief")),
		BriefLength:     int64(len("brief")),
	}
}

func fixtureProfile(request task.TaskRecord) provider.PreparedProfile {
	return provider.PreparedProfile{
		Plan:            execution.Plan{Executable: "/usr/bin/true", Directory: request.CanonicalCwd, Predicate: task.FixturePredicateRef()},
		ObservedVersion: "fixture-v2",
		Effective:       task.EffectiveConfig{Containment: "finite-fixture-process", Approval: "fixture-no-tools", Digest: task.ComputeSHA256([]byte("base"))},
	}
}

func TestLoadConfigStrictlyBindsHelperAndEnvironment(t *testing.T) {
	fixture := newFixtureFiles(t)
	loaded, err := loadConfig(fixture.outerPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SHA256 != task.ComputeSHA256(loaded.Raw) || loaded.Config.HelperConfig != fixture.helperConfig {
		t.Fatalf("loaded=%+v", loaded)
	}
	if _, err = LoadConfig(fixture.outerPath); err != nil {
		t.Fatal(err)
	}

	raw := writeFixtureJSON(t, fixture.outerPath, struct {
		SchemaVersion      int      `json:"schema_version"`
		HelperExecutable   string   `json:"helper_executable"`
		HelperSHA256       string   `json:"helper_sha256"`
		HelperConfig       string   `json:"helper_config"`
		HelperConfigSHA256 string   `json:"helper_config_sha256"`
		Environment        []string `json:"Environment"`
	}{fixture.outer.SchemaVersion, fixture.outer.HelperExecutable, fixture.outer.HelperSHA256, fixture.outer.HelperConfig, fixture.outer.HelperConfigSHA256, fixture.outer.Environment})
	if _, err = loadConfig(fixture.outerPath); err == nil {
		t.Fatalf("wrong-case environment accepted: %s", raw)
	}

	fixture.outer.Environment = []string{"DUP=one", "DUP=two"}
	writeFixtureJSON(t, fixture.outerPath, fixture.outer)
	if _, err = loadConfig(fixture.outerPath); err == nil {
		t.Fatal("duplicate environment accepted")
	}

	fixture.outer.Environment = nil
	writeFixtureJSON(t, fixture.outerPath, fixture.outer)
	if _, err = loadConfig(fixture.outerPath); err == nil {
		t.Fatal("implicit environment accepted")
	}
}

func TestConfigAndHelperRejectPathDigestAndDelayFaults(t *testing.T) {
	fixture := newFixtureFiles(t)
	cases := []struct {
		name string
		edit func(*Config, *HelperConfig)
	}{
		{name: "helper digest", edit: func(cfg *Config, _ *HelperConfig) { cfg.HelperSHA256 = strings.Repeat("0", 64) }},
		{name: "helper config digest", edit: func(cfg *Config, _ *HelperConfig) { cfg.HelperConfigSHA256 = strings.Repeat("0", 64) }},
		{name: "helper config delay", edit: func(_ *Config, helper *HelperConfig) { helper.DelayMS = MaxDelay.Milliseconds() + 1 }},
		{name: "helper ineligible", edit: func(_ *Config, helper *HelperConfig) { helper.Eligible = false }},
		{name: "unclean artifact", edit: func(_ *Config, helper *HelperConfig) { helper.ArtifactDir += string(filepath.Separator) + "." }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			current := fixture.outer
			helper := fixture.helper
			test.edit(&current, &helper)
			writeFixtureJSON(t, fixture.helperConfig, helper)
			writeFixtureJSON(t, fixture.outerPath, current)
			if _, err := loadConfig(fixture.outerPath); err == nil {
				t.Fatal("invalid fixture accepted")
			}
		})
	}

	oversized := bytes.Repeat([]byte("x"), task.MaxControlRecordSize+1)
	writeFixtureFile(t, fixture.outerPath, oversized, 0o600)
	if _, err := loadConfig(fixture.outerPath); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("oversized config error=%v", err)
	}
}

func TestRunHelperWritesBoundedNaturalMarkersAndPrivateStreams(t *testing.T) {
	fixture := newFixtureFiles(t)
	var stdout, stderr bytes.Buffer
	if err := RunHelper([]string{fixture.helperConfig}, strings.NewReader("ignored"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var output helperOutput
	if err := task.DecodeStrict(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.SchemaVersion != SchemaVersion || !output.Eligible || output.Sentinel != string(fixture.sentinel) {
		t.Fatalf("output=%+v", output)
	}
	if !bytes.Equal(stderr.Bytes(), fixture.sentinel) {
		t.Fatalf("stderr did not carry sentinel: %q", stderr.Bytes())
	}
	invocations, err := os.ReadDir(filepath.Join(fixture.artifactDir, "invocations"))
	if err != nil || len(invocations) != 1 {
		t.Fatalf("invocations err=%v entries=%d", err, len(invocations))
	}
	invocation := filepath.Join(fixture.artifactDir, "invocations", invocations[0].Name())
	for _, marker := range []string{"started.json", "completed.json"} {
		if _, err = os.Stat(filepath.Join(invocation, marker)); err != nil {
			t.Fatalf("missing %s: %v", marker, err)
		}
	}
	if _, err = projectHelper(stdout.Bytes(), task.ComputeSHA256(fixture.sentinel), true); err != nil {
		t.Fatal(err)
	}
}

func TestRunHelperShortWriterRefusesWithoutCompletionMarker(t *testing.T) {
	fixture := newFixtureFiles(t)
	short := shortWriter{}
	var stderr bytes.Buffer
	if err := RunHelper([]string{fixture.helperConfig}, nil, short, &stderr); !errors.Is(err, ErrHelperOutput) {
		t.Fatalf("short stdout error=%v", err)
	}
	invocations, err := os.ReadDir(filepath.Join(fixture.artifactDir, "invocations"))
	if err != nil || len(invocations) != 1 {
		t.Fatalf("invocations err=%v entries=%d", err, len(invocations))
	}
	if _, err = os.Stat(filepath.Join(fixture.artifactDir, "invocations", invocations[0].Name(), "completed.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completion marker exists after output fault: %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	return len(data) - 1, nil
}

func TestPrepareInspectionCandidateFinalizesOnlyProjectedFacts(t *testing.T) {
	fixture := newFixtureFiles(t)
	request := fixtureRequest(fixture.dir)
	candidate, err := prepareInspectionCandidate(request, fixtureProfile(request), fixture.outerPath)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Inspection == nil || candidate.Inspection.Executable != fixture.helperPath || !reflect.DeepEqual(candidate.Inspection.Arguments, []string{fixture.helperConfig}) {
		t.Fatalf("candidate inspection=%+v", candidate.Inspection)
	}
	native, err := task.MarshalCanonical(helperOutput{SchemaVersion: SchemaVersion, Sentinel: string(fixture.sentinel), Eligible: true})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := candidate.Inspection.Project(native)
	if err != nil {
		t.Fatal(err)
	}
	if string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("facts=%q", facts)
	}
	prepared, err := candidate.Finalize(facts, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Effective.Digest == fixtureProfile(request).Effective.Digest {
		t.Fatal("inspection facts did not bind effective digest")
	}
	prepared.Plan.Arguments = []string{"mutated"}
	again, err := candidate.Finalize(facts, time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Plan.Arguments, fixtureProfile(request).Plan.Arguments) || again.Effective.Digest != prepared.Effective.Digest {
		t.Fatal("finalization retained caller mutation or observation time")
	}
	for _, invalid := range []json.RawMessage{nil, json.RawMessage(`{"eligible":false}`), json.RawMessage(`{"Eligible":true}`), json.RawMessage(`{"eligible":true,"extra":1}`)} {
		if _, err = candidate.Finalize(invalid, time.Now()); !errors.Is(err, provider.ErrProfileUnavailable) {
			t.Fatalf("invalid facts=%q err=%v", invalid, err)
		}
	}
}

func TestPrepareInspectionCandidateRejectsChangedHelper(t *testing.T) {
	fixture := newFixtureFiles(t)
	request := fixtureRequest(fixture.dir)
	writeFixtureFile(t, fixture.helperPath, []byte("changed helper\n"), 0o700)
	if _, err := prepareInspectionCandidate(request, fixtureProfile(request), fixture.outerPath); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("changed helper accepted: %v", err)
	}
}

func TestDependenciesDisableUnownedRecorderCallbacks(t *testing.T) {
	deps := NewDependenciesForExecutable("")
	if deps.SupervisorOptions.Observer != nil || deps.ExecutionHooks.Event != nil || deps.ExecutionHooks.Start != nil || deps.ExecutionHooks.Wait != nil || deps.ExecutionHooks.Capture != nil {
		t.Fatal("inspection fixture retained supervisor recorder callbacks")
	}
}

func TestWriteAllRejectsBrokenWriters(t *testing.T) {
	if err := writeAll(errorWriter{}, []byte("x")); !errors.Is(err, ErrHelperOutput) {
		t.Fatalf("writer error=%v", err)
	}
	if err := writeAll(nil, nil); err == nil {
		t.Fatal("nil writer accepted")
	}
	if err := writeAll(io.Discard, nil); err != nil {
		t.Fatal(err)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
