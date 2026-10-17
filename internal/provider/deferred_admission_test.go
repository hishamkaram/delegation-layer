package provider

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type deferredAdmissionFixture struct {
	request    task.TaskRecord
	definition InspectionDefinition
	capability string
	roots      []string
	pending    PreparedProfile
	finalized  PreparedProfile
}

func newDeferredAdmissionFixture(t *testing.T) deferredAdmissionFixture {
	t.Helper()
	request := artifactRequest()
	predicate := task.PredicateRef{
		Adapter: request.Provider,
		Mode:    request.Mode,
		Version: "1",
		SHA256:  task.ComputeSHA256([]byte(request.Provider + "/" + request.Mode)),
	}
	definition, err := NewRuntimeInspectionDefinition(
		CLIInfo{Path: "/bin/provider", SHA256: task.ComputeSHA256([]byte("provider-binary"))},
		request.CanonicalCwd,
		[]string{"LANG=C"},
		RuntimeCapability{VersionArgs: []string{"--version"}, HelpArgs: []string{"--help"}, RequiredFlags: []string{"--provider-test"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := RuntimeCapabilityDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	roots := []string{"/runtime"}
	plan := artifactPlan(request)
	plan.Executable = definition.Executable
	plan.ExecutableSHA256 = definition.ExecutableSHA256
	plan.Environment = slices.Clone(definition.Environment)
	plan.Predicate = predicate
	effective := deferredAdmissionEffectiveConfig(request, roots)
	pending := PreparedProfile{
		Plan:                   plan,
		RuntimePreflightSHA256: capability,
		Effective:              task.CloneEffectiveConfig(effective),
		WritableRoots:          slices.Clone(roots),
	}
	finalized := cloneCandidateProfile(pending)
	finalized.RuntimePreflightSHA256 = ""
	finalized.ObservedVersion = "provider-v1"
	finalized.Plan.ProviderRuntime = task.ProviderRuntimeIdentity{
		Executable: finalized.Plan.Executable,
		Version:    finalized.ObservedVersion,
		SHA256:     finalized.Plan.ExecutableSHA256,
	}
	return deferredAdmissionFixture{
		request: request, definition: definition, capability: capability,
		roots: roots, pending: pending, finalized: finalized,
	}
}

func deferredAdmissionEffectiveConfig(request task.TaskRecord, roots []string) task.EffectiveConfig {
	return task.EffectiveConfig{
		Containment: "read-only",
		Approval:    "never",
		Digest:      task.ComputeSHA256([]byte("effective-policy")),
		Policy: &task.PolicyDetails{
			ProfileRevision: "runtime-policy-v1",
			RuntimeSHA256:   task.ComputeSHA256([]byte("provider-runtime")),
			Workspace:       request.CanonicalCwd,
			WritableRoots:   slices.Clone(roots),
			Sources: []task.PolicySourceDigest{{
				Path: "/etc/provider/policy", Kind: "provider-policy", Present: true,
				SHA256: task.ComputeSHA256([]byte("policy-source")),
			}},
		},
	}
}

func (f deferredAdmissionFixture) candidate(admitProfile, finalizeProfile PreparedProfile) ProfileCandidate {
	definition := f.definition
	return ProfileCandidate{
		Directory:     f.request.CanonicalCwd,
		WritableRoots: slices.Clone(f.roots),
		Inspection:    &definition,
		Admit: func() (PreparedProfile, error) {
			return cloneCandidateProfile(admitProfile), nil
		},
		Finalize: func(json.RawMessage, time.Time) (PreparedProfile, error) {
			return cloneCandidateProfile(finalizeProfile), nil
		},
	}
}

func deferredAdmissionMeta(profile PreparedProfile, pending bool) task.MetaRecord {
	meta := task.MetaRecord{
		ProviderExecutable:     profile.Plan.Executable,
		ProviderVersion:        profile.ObservedVersion,
		RuntimePreflightSHA256: profile.RuntimePreflightSHA256,
		EffectiveConfig:        task.CloneEffectiveConfig(profile.Effective),
		Containment:            profile.Effective.Containment,
		Approval:               profile.Effective.Approval,
		Environment:            slices.Clone(profile.Plan.Environment),
		EnvironmentRecorded:    true,
		Predicate:              profile.Plan.Predicate,
		InputFiles:             slices.Clone(profile.Plan.InputFiles),
		OutputArtifacts:        slices.Clone(profile.Plan.OutputArtifacts),
		OutputWriterContract:   profile.Plan.OutputWriterContract,
	}
	if pending {
		meta.ProviderVersion = ""
	} else {
		meta.RuntimePreflightSHA256 = ""
	}
	return meta
}

func TestPreparedProfileDeferredAdmissionRejectsInventedObservation(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	if fixture.pending.ObservedVersion != "" || fixture.pending.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{}) {
		t.Fatalf("pending profile invented runtime observation: %+v", fixture.pending)
	}
	if err := fixture.pending.ValidateAdmission(fixture.request); err != nil {
		t.Fatalf("valid pending profile was rejected: %v", err)
	}
	withPlanMarker := cloneCandidateProfile(fixture.pending)
	withPlanMarker.Plan.RuntimePreflightSHA256 = fixture.capability
	if err := withPlanMarker.ValidateAdmission(fixture.request); err != nil {
		t.Fatalf("pending profile with the derived plan digest was rejected: %v", err)
	}
	if err := fixture.pending.Validate(fixture.request); err == nil {
		t.Fatal("strict validation accepted a profile without observed runtime facts")
	}

	cases := []struct {
		name   string
		mutate func(*PreparedProfile)
	}{
		{
			name: "invalid preflight digest",
			mutate: func(profile *PreparedProfile) {
				profile.RuntimePreflightSHA256 = "invalid"
			},
		},
		{
			name: "missing preflight digest",
			mutate: func(profile *PreparedProfile) {
				profile.RuntimePreflightSHA256 = ""
			},
		},
		{
			name: "observed version",
			mutate: func(profile *PreparedProfile) {
				profile.ObservedVersion = "provider-v1"
			},
		},
		{
			name: "provider runtime observation",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.ProviderRuntime = task.ProviderRuntimeIdentity{Executable: "/bin/provider", Version: "provider-v1", SHA256: profile.Plan.ExecutableSHA256}
			},
		},
		{
			name: "relative executable",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Executable = "provider"
			},
		},
		{
			name: "invalid executable digest",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.ExecutableSHA256 = "invalid"
			},
		},
		{
			name: "ambient environment",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Environment = nil
			},
		},
		{
			name: "mismatched plan digest",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("different-capability"))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := cloneCandidateProfile(fixture.pending)
			testCase.mutate(&profile)
			if err := profile.ValidateAdmission(fixture.request); err == nil {
				t.Fatal("invalid pending profile was accepted")
			}
		})
	}
}

func TestCatalogDeferredAdmissionDerivesPlanDigestAndKeepsFinalizerStrict(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	registration := testRegistration(fixture.request.Provider, fixture.request.Mode)
	registration.Prepare = func(task.TaskRecord) (ProfileCandidate, error) {
		return fixture.candidate(fixture.pending, fixture.pending), nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.Candidate(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, finalizeErr := candidate.Finalize(nil, time.Unix(2_000_000_000, 0)); finalizeErr == nil {
		t.Fatal("strict catalog finalizer invented an observed runtime")
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatalf("valid static admission was rejected: %v", err)
	}
	if profile.RuntimePreflightSHA256 != fixture.capability || profile.Plan.RuntimePreflightSHA256 != fixture.capability {
		t.Fatalf("runtime preflight digest was not derived into the plan: profile=%+v want=%s", profile, fixture.capability)
	}
	if profile.ObservedVersion != "" || profile.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{}) {
		t.Fatalf("static admission invented runtime observation: %+v", profile)
	}
	if err := profile.ValidateAdmission(fixture.request); err != nil {
		t.Fatalf("catalog returned an invalid pending profile: %v", err)
	}
}

func TestAdmissionProfileRejectsWrongRuntimeLaunchIdentityWithoutAuthority(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	cases := []struct {
		name   string
		mutate func(*PreparedProfile)
	}{
		{
			name: "executable",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Executable = "/opt/other-provider"
			},
		},
		{
			name: "executable digest",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.ExecutableSHA256 = task.ComputeSHA256([]byte("other-provider"))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pending := cloneCandidateProfile(fixture.pending)
			testCase.mutate(&pending)
			candidate := fixture.candidate(pending, fixture.finalized)
			profile, err := candidate.AdmissionProfile(fixture.request)
			if !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("wrong runtime launch identity error=%v, want identity mismatch", err)
			}
			if !reflect.DeepEqual(profile, PreparedProfile{}) {
				t.Fatalf("rejected pending profile retained authority: %+v", profile)
			}
		})
	}
}

func TestCatalogFinalizerRejectsWrongRuntimeLaunchIdentityWithoutAuthority(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	cases := []struct {
		name   string
		mutate func(*PreparedProfile)
	}{
		{
			name: "executable",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Executable = "/opt/other-provider"
			},
		},
		{
			name: "executable digest",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.ExecutableSHA256 = task.ComputeSHA256([]byte("other-provider"))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			finalized := cloneCandidateProfile(fixture.finalized)
			testCase.mutate(&finalized)
			registration := testRegistration(fixture.request.Provider, fixture.request.Mode)
			registration.Prepare = func(task.TaskRecord) (ProfileCandidate, error) {
				return fixture.candidate(fixture.pending, finalized), nil
			}
			catalog, err := NewCatalog(registration)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := catalog.Candidate(fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := candidate.Finalize(nil, time.Unix(2_000_000_000, 0))
			if !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("wrong finalized runtime launch identity error=%v, want identity mismatch", err)
			}
			if !reflect.DeepEqual(profile, PreparedProfile{}) {
				t.Fatalf("rejected finalized profile retained authority: %+v", profile)
			}
		})
	}
}

func TestCatalogDeferredAdmissionBindsRuntimeAndPredicateContracts(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	cases := []struct {
		name    string
		mutate  func(*PreparedProfile)
		wantErr error
	}{
		{
			name: "capability digest",
			mutate: func(profile *PreparedProfile) {
				profile.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("different-capability"))
			},
			wantErr: task.ErrIdentityMismatch,
		},
		{
			name: "directory",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Directory = "/other-workspace"
			},
			wantErr: task.ErrIdentityMismatch,
		},
		{
			name: "writable roots",
			mutate: func(profile *PreparedProfile) {
				profile.WritableRoots = []string{"/other-runtime"}
			},
			wantErr: task.ErrIdentityMismatch,
		},
		{
			name: "registered predicate",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Predicate = task.PredicateRef{Adapter: fixture.request.Provider, Mode: fixture.request.Mode, Version: "unknown", SHA256: task.ComputeSHA256([]byte("unknown-predicate"))}
			},
			wantErr: ErrProfileUnavailable,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			registration := testRegistration(fixture.request.Provider, fixture.request.Mode)
			admit := cloneCandidateProfile(fixture.pending)
			testCase.mutate(&admit)
			registration.Prepare = func(task.TaskRecord) (ProfileCandidate, error) {
				return fixture.candidate(admit, fixture.pending), nil
			}
			catalog, err := NewCatalog(registration)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := catalog.Candidate(fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			_, err = candidate.Admit()
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("admission error=%v, want %v", err, testCase.wantErr)
			}
		})
	}

	registration := testRegistration(fixture.request.Provider, fixture.request.Mode)
	registration.Prepare = func(task.TaskRecord) (ProfileCandidate, error) {
		candidate := fixture.candidate(fixture.pending, fixture.pending)
		mixed := fixture.definition
		mixed.Arguments = []string{"--native"}
		mixed.Project = func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
		candidate.Inspection = &mixed
		return candidate, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Candidate(fixture.request); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("non-runtime admission candidate was accepted: %v", err)
	}
}

func TestCatalogDeferredAdmissionNormalizesAndDeeplyIsolatesProfiles(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	pending := cloneCandidateProfile(fixture.pending)
	pending.Plan.Arguments = append(pending.Plan.Arguments, "")
	pending.Plan.InputFiles = []task.InputFile{
		{Name: "settings.json", ArgumentIndex: 1, Content: `{"mode":"read-only"}`},
		{Name: "a.json", ArgumentIndex: 3, Content: "a"},
	}
	pending.Plan.OutputArtifacts = []task.OutputArtifact{
		{Name: "z-output", ArgumentIndex: 7},
		{Name: "answer.txt", ArgumentIndex: 5},
	}
	finalized := cloneCandidateProfile(fixture.finalized)
	finalized.Plan.Arguments = slices.Clone(pending.Plan.Arguments)
	finalized.Plan.InputFiles = slices.Clone(pending.Plan.InputFiles)
	finalized.Plan.OutputArtifacts = slices.Clone(pending.Plan.OutputArtifacts)

	candidate := requireCatalogDeferredCandidate(t, fixture, pending, finalized)
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatalf("static admission was rejected: %v", err)
	}
	prepared, err := candidate.Finalize(nil, time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatalf("normal finalizer was rejected: %v", err)
	}
	wantInputs := []task.InputFile{
		{Name: "a.json", ArgumentIndex: 3, Content: "a"},
		{Name: "settings.json", ArgumentIndex: 1, Content: `{"mode":"read-only"}`},
	}
	wantOutputs := []task.OutputArtifact{
		{Name: "answer.txt", ArgumentIndex: 5},
		{Name: "z-output", ArgumentIndex: 7},
	}
	assertDeferredArtifactNormalization(t, admitted, prepared, wantInputs, wantOutputs)

	mutateDeferredAdmissionResult(&admitted)
	second, err := candidate.Admit()
	if err != nil {
		t.Fatalf("second static admission was rejected: %v", err)
	}
	assertDeferredAdmissionIsolation(t, second)
}

func requireCatalogDeferredCandidate(t *testing.T, fixture deferredAdmissionFixture, admitProfile, finalizeProfile PreparedProfile) ProfileCandidate {
	t.Helper()
	registration := testRegistration(fixture.request.Provider, fixture.request.Mode)
	registration.Prepare = func(task.TaskRecord) (ProfileCandidate, error) {
		return fixture.candidate(admitProfile, finalizeProfile), nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.Candidate(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func assertDeferredArtifactNormalization(t *testing.T, admitted, finalized PreparedProfile, wantInputs []task.InputFile, wantOutputs []task.OutputArtifact) {
	t.Helper()
	if !reflect.DeepEqual(admitted.Plan.InputFiles, wantInputs) || !reflect.DeepEqual(finalized.Plan.InputFiles, wantInputs) {
		t.Fatalf("common input normalization mismatch: admitted=%+v finalized=%+v", admitted.Plan.InputFiles, finalized.Plan.InputFiles)
	}
	if !reflect.DeepEqual(admitted.Plan.OutputArtifacts, wantOutputs) || !reflect.DeepEqual(finalized.Plan.OutputArtifacts, wantOutputs) {
		t.Fatalf("common output normalization mismatch: admitted=%+v finalized=%+v", admitted.Plan.OutputArtifacts, finalized.Plan.OutputArtifacts)
	}
}

func mutateDeferredAdmissionResult(profile *PreparedProfile) {
	profile.Plan.Arguments[1] = "mutated"
	profile.Plan.Environment[0] = "LANG=mutated"
	profile.Plan.InputFiles[0].Content = "mutated"
	profile.Plan.OutputArtifacts[0].Name = "mutated"
	profile.WritableRoots[0] = "/mutated-runtime"
	profile.Effective.Policy.WritableRoots[0] = "/mutated-runtime"
}

func assertDeferredAdmissionIsolation(t *testing.T, profile PreparedProfile) {
	t.Helper()
	if profile.Plan.Arguments[1] != "" || profile.Plan.Environment[0] != "LANG=C" || profile.Plan.InputFiles[0].Content != "a" || profile.Plan.OutputArtifacts[0].Name != "answer.txt" || profile.WritableRoots[0] != "/runtime" || profile.Effective.Policy.WritableRoots[0] != "/runtime" {
		t.Fatalf("admission result was not deeply isolated: %+v", profile)
	}
}

type preparedProfileMutationCase struct {
	name            string
	mutate          func(*PreparedProfile, *task.MetaRecord)
	wantIdentityErr bool
}

func TestPreparedProfileMatchesRequiresExactPendingMarkerAndImmutablePolicy(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	profile := cloneCandidateProfile(fixture.pending)
	meta := deferredAdmissionMeta(profile, true)
	if err := profile.Matches(fixture.request, meta); err != nil {
		t.Fatalf("valid pending profile did not match exact metadata: %v", err)
	}

	cases := []preparedProfileMutationCase{
		{
			name: "missing pending marker",
			mutate: func(_ *PreparedProfile, meta *task.MetaRecord) {
				meta.RuntimePreflightSHA256 = ""
			},
		},
		{
			name:            "different pending marker",
			wantIdentityErr: true,
			mutate: func(_ *PreparedProfile, meta *task.MetaRecord) {
				meta.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("different-capability"))
			},
		},
		{
			name:            "environment",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Plan.Environment = []string{"LANG=C", "CHANGED=1"}
			},
		},
		{
			name:            "effective policy",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Effective.Digest = task.ComputeSHA256([]byte("changed-policy"))
			},
		},
		{
			name:            "policy workspace",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Effective.Policy.Workspace = "/other-workspace"
			},
		},
		{
			name:            "predicate",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Plan.Predicate.Version = "2"
				profile.Plan.Predicate.SHA256 = task.ComputeSHA256([]byte("changed-predicate"))
			},
		},
		{
			name:            "input artifact",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Plan.InputFiles[0].Content = "changed"
			},
		},
		{
			name:            "output writer",
			wantIdentityErr: true,
			mutate: func(_ *PreparedProfile, meta *task.MetaRecord) {
				meta.OutputWriterContract = ""
			},
		},
	}
	assertPreparedProfileMutationCases(t, fixture.request, profile, meta, cases, func(profile *PreparedProfile, meta *task.MetaRecord) error {
		return profile.Matches(fixture.request, *meta)
	})
}

func TestPreparedProfileMatchesWithRuntimeRefreshPreservesImmutableContract(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	meta := deferredAdmissionMeta(fixture.finalized, false)
	refreshed := cloneCandidateProfile(fixture.finalized)
	refreshed.Plan.Executable = "/opt/provider-v2"
	refreshed.Plan.ExecutableSHA256 = task.ComputeSHA256([]byte("provider-v2"))
	refreshed.ObservedVersion = "provider-v2"
	refreshed.Plan.ProviderRuntime = task.ProviderRuntimeIdentity{
		Executable: refreshed.Plan.Executable,
		Version:    refreshed.ObservedVersion,
		SHA256:     refreshed.Plan.ExecutableSHA256,
	}
	refreshed.Effective.Digest = task.ComputeSHA256([]byte("effective-policy-v2"))
	refreshed.Effective.Policy.RuntimeSHA256 = task.ComputeSHA256([]byte("provider-runtime-v2"))
	if err := refreshed.Matches(fixture.request, meta); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("strict startup matching accepted a changed runtime: %v", err)
	}
	if err := refreshed.MatchesWithRuntimeRefresh(fixture.request, meta); err != nil {
		t.Fatalf("valid observed runtime refresh was rejected: %v", err)
	}

	cases := runtimeRefreshProfileMutationCases()
	assertPreparedProfileMutationCases(t, fixture.request, refreshed, meta, cases, func(profile *PreparedProfile, meta *task.MetaRecord) error {
		return profile.MatchesWithRuntimeRefresh(fixture.request, *meta)
	})
}

func runtimeRefreshProfileMutationCases() []preparedProfileMutationCase {
	return []preparedProfileMutationCase{
		{
			name:            "environment",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Plan.Environment = []string{"LANG=C", "CHANGED=1"}
			},
		},
		{
			name:            "containment",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Effective.Containment = "workspace-write"
			},
		},
		{
			name:            "approval",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Effective.Approval = "accept-edits"
			},
		},
		{
			name:            "policy workspace",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Effective.Policy.Workspace = "/other-workspace"
			},
		},
		{
			name:            "policy writable roots",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.WritableRoots = []string{"/other-runtime"}
				profile.Effective.Policy.WritableRoots = []string{"/other-runtime"}
			},
		},
		{
			name:            "policy source",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Effective.Policy.Sources[0].SHA256 = task.ComputeSHA256([]byte("changed-source"))
			},
		},
		{
			name:            "predicate",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Plan.Predicate.Version = "2"
				profile.Plan.Predicate.SHA256 = task.ComputeSHA256([]byte("changed-predicate"))
			},
		},
		{
			name:            "input artifact",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Plan.InputFiles[0].Content = "changed"
			},
		},
		{
			name:            "output artifact",
			wantIdentityErr: true,
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.Plan.OutputArtifacts[0].Name = "changed-output"
			},
		},
		{
			name:            "writer contract",
			wantIdentityErr: true,
			mutate: func(_ *PreparedProfile, meta *task.MetaRecord) {
				meta.OutputWriterContract = ""
			},
		},
	}
}

func assertPreparedProfileMutationCases(
	t *testing.T,
	request task.TaskRecord,
	profile PreparedProfile,
	meta task.MetaRecord,
	cases []preparedProfileMutationCase,
	match func(*PreparedProfile, *task.MetaRecord) error,
) {
	t.Helper()
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			current := cloneCandidateProfile(profile)
			currentMeta := meta
			testCase.mutate(&current, &currentMeta)
			err := match(&current, &currentMeta)
			if err == nil {
				t.Fatal("immutable launch contract was accepted")
			}
			if testCase.wantIdentityErr && !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("immutable launch contract error=%v, want identity mismatch", err)
			}
		})
	}
}

func TestPreparedProfileMatchesPendingRuntimeRefreshAllowsCompatibleUpgrade(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	meta := deferredAdmissionMeta(fixture.pending, true)
	refreshed := pendingRuntimeRefreshProfile(fixture)
	if err := refreshed.MatchesPendingRuntimeRefresh(fixture.request, meta); err != nil {
		t.Fatalf("compatible pending runtime refresh was rejected: %v", err)
	}
	if refreshed.RuntimePreflightSHA256 != fixture.capability || refreshed.Plan.RuntimePreflightSHA256 != fixture.capability || refreshed.ObservedVersion != "" || refreshed.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{}) {
		t.Fatalf("pending runtime authority changed during matching: %+v", refreshed)
	}
	if meta.RuntimePreflightSHA256 != fixture.capability || meta.ProviderVersion != "" {
		t.Fatalf("pending metadata lost its marker or empty version: %+v", meta)
	}
	if err := refreshed.MatchesWithRuntimeRefresh(fixture.request, meta); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("strict launch matching accepted a pending compatible refresh: %v", err)
	}
}

func TestPreparedProfileMatchesPendingRuntimeRefreshPreservesImmutableContract(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	meta := deferredAdmissionMeta(fixture.pending, true)
	// Raw argv consistency belongs to execution preflight in TestPendingRuntimeRefreshPreservesAdmittedLaunchContract; MetaRecord owns these persisted artifact bindings.
	cases := []struct {
		name   string
		mutate func(*PreparedProfile)
	}{
		{
			name: "input artifact binding",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.InputFiles[0].ArgumentIndex = 3
			},
		},
		{
			name: "output artifact binding",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.OutputArtifacts[0].ArgumentIndex = 3
			},
		},
		{
			name: "policy",
			mutate: func(profile *PreparedProfile) {
				profile.Effective.Policy.Workspace = "/other-workspace"
			},
		},
		{
			name: "environment",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Environment = []string{"LANG=C", "CHANGED=1"}
			},
		},
		{
			name: "predicate",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.Predicate.Version = "2"
				profile.Plan.Predicate.SHA256 = task.ComputeSHA256([]byte("changed-predicate"))
			},
		},
		{
			name: "outputs",
			mutate: func(profile *PreparedProfile) {
				profile.Plan.OutputArtifacts[0].Name = "changed-output"
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			refreshed := pendingRuntimeRefreshProfile(fixture)
			testCase.mutate(&refreshed)
			if err := refreshed.MatchesPendingRuntimeRefresh(fixture.request, meta); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("immutable pending contract error=%v, want identity mismatch", err)
			}
		})
	}
}

func TestPreparedProfileMatchesPendingRuntimeRefreshRequiresExactMarker(t *testing.T) {
	fixture := newDeferredAdmissionFixture(t)
	profile := pendingRuntimeRefreshProfile(fixture)
	meta := deferredAdmissionMeta(fixture.pending, true)
	cases := []struct {
		name   string
		mutate func(*PreparedProfile, *task.MetaRecord)
	}{
		{
			name: "profile marker missing",
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.RuntimePreflightSHA256 = ""
			},
		},
		{
			name: "metadata marker missing",
			mutate: func(_ *PreparedProfile, meta *task.MetaRecord) {
				meta.RuntimePreflightSHA256 = ""
			},
		},
		{
			name: "different marker",
			mutate: func(profile *PreparedProfile, _ *task.MetaRecord) {
				profile.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("different-capability"))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			current := cloneCandidateProfile(profile)
			currentMeta := meta
			testCase.mutate(&current, &currentMeta)
			if err := current.MatchesPendingRuntimeRefresh(fixture.request, currentMeta); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("invalid pending marker error=%v, want identity mismatch", err)
			}
		})
	}
}

func pendingRuntimeRefreshProfile(fixture deferredAdmissionFixture) PreparedProfile {
	refreshed := cloneCandidateProfile(fixture.pending)
	refreshed.Plan.RuntimePreflightSHA256 = fixture.capability
	refreshed.Plan.Executable = "/opt/provider-v2"
	refreshed.Plan.ExecutableSHA256 = task.ComputeSHA256([]byte("provider-v2"))
	refreshed.Effective.Digest = task.ComputeSHA256([]byte("effective-policy-v2"))
	refreshed.Effective.Policy.RuntimeSHA256 = task.ComputeSHA256([]byte("provider-runtime-v2"))
	return refreshed
}
