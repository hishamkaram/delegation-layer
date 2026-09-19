package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

func fixtureSeal(t *testing.T, stdout []byte) *ProviderExitRecord {
	t.Helper()
	manifest := []RawManifestEntry{
		{Path: "raw/stderr", Size: 0, SHA256: ComputeSHA256(nil)},
		{Path: "raw/stdout", Size: int64(len(stdout)), SHA256: ComputeSHA256(stdout)},
	}
	data, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return &ProviderExitRecord{
		SchemaVersion: SchemaVersion, RootID: strings.Repeat("1", 32), TaskID: strings.Repeat("2", 32),
		SpecSHA256: ComputeSHA256([]byte("spec")), MetaSHA256: ComputeSHA256([]byte("meta")),
		InvocationState: InvocationStarted, ExitCode: 0, Predicate: FixturePredicateRef(),
		RawManifest: manifest, ManifestSHA256: ComputeSHA256(data), ClosedAt: "2026-09-13T00:00:00Z",
	}
}

func fixtureTask(t *testing.T) TaskRecord {
	t.Helper()
	return TaskRecord{
		SchemaVersion: SchemaVersion, RootID: strings.Repeat("1", 32), TaskID: strings.Repeat("2", 32),
		Provider: config.ProviderFixture, Mode: config.ModeReadOnly, CanonicalCwd: "/fixture/workspace",
		RequestedConfig: TaskConfig{Permission: config.ModeReadOnly, Budget: "1s"}, BudgetNanos: int64(time.Second),
		BriefSHA256: ComputeSHA256([]byte("brief")), BriefLength: 5,
	}
}

func fixtureMeta(t *testing.T) MetaRecord {
	t.Helper()
	record := fixtureTask(t)
	data, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	return MetaRecord{
		SchemaVersion: SchemaVersion, RootID: record.RootID, TaskID: record.TaskID, SpecSHA256: ComputeSHA256(data),
		RequestedConfig: record.RequestedConfig, Predicate: FixturePredicateRef(),
		EffectiveConfig: EffectiveConfig{Containment: "fixture-local", Approval: "never", Digest: ComputeSHA256([]byte("fixture-config"))},
		Containment:     "fixture-local", Approval: "never", ProviderExecutable: "/fixture/protocolfixture", ProviderVersion: "fixture-v1",
		PublisherBuild: "fixture-build", PublisherVersion: "1", CreatedAt: "2026-09-13T00:00:00Z",
		SupervisorConfig: SupervisorRef{ConfigPath: "/fixture/supervisor.json", ConfigDigest: ComputeSHA256([]byte("supervisor-config")), Endpoint: "fixture://supervisor", ObservedVersion: "fixture-v1"},
	}
}

func TestTaskConfigurationBoundary(t *testing.T) {
	valid := fixtureTask(t)
	if err := ValidateTaskRecord(&valid); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*TaskRecord)
	}{
		{"provider", func(r *TaskRecord) { r.Provider = "unknown" }},
		{"mode", func(r *TaskRecord) { r.Mode = "unrestricted" }},
		{"permission", func(r *TaskRecord) { r.RequestedConfig.Permission = "workspace-write" }},
		{"missing-budget", func(r *TaskRecord) { r.RequestedConfig.Budget = "" }},
		{"invalid-budget", func(r *TaskRecord) { r.RequestedConfig.Budget = "not-duration" }},
		{"budget-mismatch", func(r *TaskRecord) { r.BudgetNanos *= 2 }},
		{"unnormalized-budget", func(r *TaskRecord) { r.RequestedConfig.Budget = "1000ms" }},
		{"relative-cwd", func(r *TaskRecord) { r.CanonicalCwd = "missing" }},
		{"unclean-cwd", func(r *TaskRecord) { r.CanonicalCwd = valid.CanonicalCwd + "/../workspace" }},
		{"nul-cwd", func(r *TaskRecord) { r.CanonicalCwd = valid.CanonicalCwd + "\x00" }},
		{"invalid-prior", func(r *TaskRecord) {
			r.PriorSession = &PriorSession{Provider: r.Provider, ConversationID: "known", PredecessorTaskID: r.TaskID}
		}},
		{"oversized-brief", func(r *TaskRecord) { r.BriefLength = MaxBriefSize + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			tc.mutate(&r)
			if err := ValidateTaskRecord(&r); err == nil {
				t.Fatal("invalid requested config accepted")
			}
		})
	}
}

func TestNormalizeRequestedConfig(t *testing.T) {
	valid := fixtureTask(t)
	input := valid
	input.Mode = ""
	input.RequestedConfig = TaskConfig{}
	input.BudgetNanos = 0
	if err := NormalizeRequestedConfig(&input); err != nil {
		t.Fatal(err)
	}
	if input.CanonicalCwd != valid.CanonicalCwd || input.Mode != config.ModeReadOnly || input.BudgetNanos != int64(config.DefaultBudgetDuration) || input.RequestedConfig.Budget != "30m0s" {
		t.Fatalf("not normalized: %+v", input)
	}
	if err := ValidateTaskRecord(&input); err != nil {
		t.Fatal(err)
	}
	input = valid
	input.RequestedConfig.Budget = "2s"
	before := input
	if err := NormalizeRequestedConfig(&input); err == nil {
		t.Fatal("budget mismatch normalized away")
	}
	if input != before {
		t.Fatal("failed normalization altered the saved request")
	}
}

func TestCompleteMetaRecord(t *testing.T) {
	valid := fixtureMeta(t)
	if err := ValidateMetaRecord(&valid); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*MetaRecord)
	}{
		{"effective-digest", func(r *MetaRecord) { r.EffectiveConfig.Digest = "" }},
		{"effective-containment", func(r *MetaRecord) { r.EffectiveConfig.Containment = "" }},
		{"effective-approval", func(r *MetaRecord) { r.EffectiveConfig.Approval = "" }},
		{"containment", func(r *MetaRecord) { r.Containment = "" }},
		{"approval", func(r *MetaRecord) { r.Approval = "" }},
		{"executable", func(r *MetaRecord) { r.ProviderExecutable = "relative" }},
		{"provider-version", func(r *MetaRecord) { r.ProviderVersion = "" }},
		{"publisher-build", func(r *MetaRecord) { r.PublisherBuild = "" }},
		{"publisher-version", func(r *MetaRecord) { r.PublisherVersion = "" }},
		{"predicate-digest", func(r *MetaRecord) { r.Predicate.SHA256 = "" }},
		{"predicate-mode", func(r *MetaRecord) { r.Predicate.Mode = "workspace-write" }},
		{"requested-budget", func(r *MetaRecord) { r.RequestedConfig.Budget = "" }},
		{"supervisor-path", func(r *MetaRecord) { r.SupervisorConfig.ConfigPath = "relative" }},
		{"supervisor-daemon-path", func(r *MetaRecord) { r.SupervisorConfig.DaemonExecutable = "relative" }},
		{"supervisor-daemon-digest", func(r *MetaRecord) { r.SupervisorConfig.DaemonSHA256 = "invalid" }},
		{"supervisor-endpoint", func(r *MetaRecord) { r.SupervisorConfig.Endpoint = "" }},
		{"supervisor-digest", func(r *MetaRecord) { r.SupervisorConfig.ConfigDigest = "" }},
		{"supervisor-version", func(r *MetaRecord) { r.SupervisorConfig.ObservedVersion = "" }},
		{"timestamp", func(r *MetaRecord) { r.CreatedAt = "yesterday" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			tc.mutate(&r)
			if err := ValidateMetaRecord(&r); err == nil {
				t.Fatal("incomplete meta accepted")
			}
		})
	}
}

func TestSealRequiredExitObservation(t *testing.T) {
	seal := fixtureSeal(t, []byte("ok"))
	data, err := MarshalCanonical(seal)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0", "9", "null", "absent"} {
		t.Run(value, func(t *testing.T) {
			fields["exit_code"] = json.RawMessage(value)
			if value == "absent" {
				delete(fields, "exit_code")
			}
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			checkExitObservation(t, encoded, value)
		})
	}
}

func checkExitObservation(t *testing.T, encoded []byte, value string) {
	t.Helper()
	var decoded ProviderExitRecord
	err := DecodeStrict(encoded, &decoded)
	if value == "null" || value == "absent" {
		if err == nil {
			t.Fatal("missing exit observation became zero")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if validateErr := ValidateProviderExitRecord(&decoded); validateErr != nil {
		t.Fatal(validateErr)
	}
	decision, err := EvaluateRegisteredPredicate(FixturePredicateRef(), &decoded)
	if err != nil {
		t.Fatal(err)
	}
	want := VerdictCommitted
	if value == "9" {
		want = VerdictRejected
	}
	if decision.Verdict != want {
		t.Fatalf("verdict %s, want %s", decision.Verdict, want)
	}
}

func TestStrictSemanticAliasesAndNestedNull(t *testing.T) {
	cases := []string{
		`{"schema_version":999,"\u017fchema_version":1}`,
		`{"outer":{"sha256":"a","\u017fha256":"b"}}`,
		`{"key":1,"\u212Aey":2}`,
	}
	for _, data := range cases {
		if err := ValidateJSONStructure([]byte(data)); !errors.Is(err, ErrDuplicateKey) {
			t.Fatalf("alias accepted %s: %v", data, err)
		}
	}
	var root RootRecord
	if err := DecodeStrict([]byte(`{"\u017fchema_version":1,"root_id":"11111111111111111111111111111111","created_at":"2026-09-13T00:00:00Z"}`), &root); err == nil {
		t.Fatal("noncanonical single alias accepted")
	}
	meta := fixtureMeta(t)
	data, err := MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"containment":"fixture-local"`), []byte(`"containment":null`), 1)
	if err := DecodeStrict(data, &meta); err == nil {
		t.Fatal("nested required null accepted")
	}
}

func TestCompleteManifestAndPredicate(t *testing.T) {
	stdout := []byte("  exact raw answer\n")
	seal := fixtureSeal(t, stdout)
	verdict, basename, answer, err := EvaluateFixturePredicate(seal, stdout)
	if err != nil || verdict != VerdictCommitted || basename != "result.txt" || !bytes.Equal(answer, stdout) {
		t.Fatalf("exact answer lost: %q %s %s %v", answer, verdict, basename, err)
	}
	if _, _, _, replaceErr := EvaluateFixturePredicate(seal, []byte("replacement")); !errors.Is(replaceErr, ErrEvidenceFault) {
		t.Fatalf("unsealed replacement accepted: %v", replaceErr)
	}
	unknown := FixturePredicateRef()
	unknown.Version = "2"
	if _, lookupErr := EvaluateRegisteredPredicate(unknown, seal); !errors.Is(lookupErr, ErrIncompatiblePredicate) {
		t.Fatalf("unknown registry entry accepted: %v", lookupErr)
	}
	for _, path := range []string{"../outside", "raw/../stdout", "raw/extra", "/raw/stdout"} {
		t.Run(path, func(t *testing.T) {
			invalid := fixtureSeal(t, stdout)
			invalid.RawManifest[1].Path = path
			rehashManifest(t, invalid)
			if validateErr := ValidateProviderExitRecord(invalid); !errors.Is(validateErr, ErrEvidenceFault) {
				t.Fatalf("escaping/undeclared evidence accepted: %v", validateErr)
			}
		})
	}
	seal.RawManifest = seal.RawManifest[:1]
	rehashManifest(t, seal)
	if validateErr := ValidateProviderExitRecord(seal); !errors.Is(validateErr, ErrEvidenceFault) {
		t.Fatalf("missing stdout accepted: %v", validateErr)
	}
	empty := fixtureSeal(t, nil)
	decision, err := EvaluateRegisteredPredicate(FixturePredicateRef(), empty)
	if err != nil || decision.Verdict != VerdictRejected || string(decision.PayloadContent) != "empty_answer" {
		t.Fatalf("empty success fabricated: %+v %v", decision, err)
	}
}

func rehashManifest(t *testing.T, seal *ProviderExitRecord) {
	t.Helper()
	data, err := MarshalCanonical(seal.RawManifest)
	if err != nil {
		t.Fatal(err)
	}
	seal.ManifestSHA256 = ComputeSHA256(data)
}

func TestTerminalOutcomeSelfContainedValidation(t *testing.T) {
	seal := fixtureSeal(t, []byte("ok"))
	valid := OutcomeRecord{SchemaVersion: SchemaVersion, RootID: seal.RootID, TaskID: seal.TaskID, SpecSHA256: seal.SpecSHA256, MetaSHA256: seal.MetaSHA256, EvidenceSHA256: seal.ManifestSHA256, Predicate: seal.Predicate, Verdict: VerdictCommitted, Payload: PayloadDescriptor{Basename: "result.txt", Length: 2, SHA256: ComputeSHA256([]byte("ok"))}}
	for _, verdict := range []string{VerdictCommitted, VerdictRejected} {
		t.Run(verdict, func(t *testing.T) {
			r := valid
			r.Verdict = verdict
			if verdict == VerdictRejected {
				r.Payload.Basename = "publish.reject"
			}
			pub, err := ReducePublication(&r, true, r.Payload.Length, r.Payload.SHA256, nil)
			if err != nil || pub.String() != verdict {
				t.Fatalf("valid terminal without seal: %s %v", pub, err)
			}
			missingPredicate := r
			missingPredicate.Predicate = PredicateRef{}
			if pub, err := ReducePublication(&missingPredicate, true, 2, r.Payload.SHA256, nil); err == nil || pub != PublicationUnknown {
				t.Fatal("missing predicate terminal")
			}
			r.Payload.Length = 0
			r.Payload.SHA256 = ComputeSHA256(nil)
			if pub, err := ReducePublication(&r, true, 0, r.Payload.SHA256, nil); err == nil || pub != PublicationUnknown {
				t.Fatal("empty payload terminal")
			}
		})
	}
}
