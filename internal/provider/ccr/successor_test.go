package ccr

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var successorModes = []string{config.ModeReadOnly, config.ModeWorkspaceWrite}

// successorRequest is a continuation of a recorded CCR session with an explicit
// model, in a workspace holding an inert ccr executable.
func successorRequest(t *testing.T, mode string) task.TaskRecord {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "ccr"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := testRequest()
	request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "recorded-session", PredecessorTaskID: strings.Repeat("c", 32)}
	return request
}

func successorMeta(request task.TaskRecord, reference task.PredicateRef) task.MetaRecord {
	return task.MetaRecord{
		Predicate:   reference,
		Environment: []string{"HOME=" + request.CanonicalCwd, "PATH=" + request.CanonicalCwd}, EnvironmentRecorded: true,
	}
}

func TestSuccessorCandidateSelectsCurrentPredicateOfTheSameLaunchShape(t *testing.T) {
	for _, mode := range successorModes {
		for _, source := range []struct {
			route  bool
			target task.PredicateRef
		}{{true, Reference(mode)}, {false, modeledReference(mode)}} {
			reference := preAttributionReference(mode, source.route)
			t.Run(mode+"/"+reference.Version, func(t *testing.T) {
				requireSuccessorSelectsTarget(t, successorRequest(t, mode), reference, source.target, source.route)
			})
		}
	}
}

// requireSuccessorSelectsTarget prepares the successor and the reconstruction
// of a record stored under reference. The successor carries target and keeps the
// recorded launch shape and inspection; the reconstruction keeps reference.
func requireSuccessorSelectsTarget(t *testing.T, request task.TaskRecord, reference, target task.PredicateRef, route bool) {
	t.Helper()
	meta := successorMeta(request, reference)
	successor, err := PrepareSuccessorCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	got, err := successor.Finalize(routeFacts(t, successor, route), time.Now())
	if err != nil {
		t.Fatalf("recorded capability was refused: %v", err)
	}
	want, err := reconstructed.Finalize(routeFacts(t, reconstructed, route), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Plan.Predicate.Equal(target) || !want.Plan.Predicate.Equal(reference) {
		t.Fatalf("successor predicate %+v (want %+v), reconstruction %+v (want %+v)", got.Plan.Predicate, target, want.Plan.Predicate, reference)
	}
	requireSameLaunch(t, got, want)
	requireLaunchShape(t, got.Plan.Arguments, request.RequestedConfig.Model, route)
	if inspectionDigest(t, successor) != inspectionDigest(t, reconstructed) {
		t.Error("successor runtime inspection differs from the recorded one")
	}
	// The recorded route capability decides the launch; the opposite proof is
	// refused rather than changing the launch shape.
	if _, err = successor.Finalize(routeFacts(t, successor, !route), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Errorf("opposite capability error=%v, want %v", err, commonprovider.ErrProfileUnavailable)
	}
}

// inspectionDigest identifies the supervised inspection definition, whose
// projection function cannot be compared directly.
func inspectionDigest(t *testing.T, candidate commonprovider.ProfileCandidate) string {
	t.Helper()
	_, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func requireSameLaunch(t *testing.T, got, want commonprovider.PreparedProfile) {
	t.Helper()
	got.Plan.Predicate, want.Plan.Predicate = task.PredicateRef{}, task.PredicateRef{}
	if !reflect.DeepEqual(got.Plan, want.Plan) || !reflect.DeepEqual(got.Effective, want.Effective) || !slices.Equal(got.WritableRoots, want.WritableRoots) {
		t.Errorf("launch differs beyond the predicate:\ngot=%+v\nwant=%+v", got, want)
	}
}

func requireLaunchShape(t *testing.T, arguments []string, model string, route bool) {
	t.Helper()
	routePrefix := []string{"launch", "--resume-route"}
	modeledPrefix := []string{"launch", "--model", model, "--auth-mode", "provider-only"}
	switch {
	case route && (!slices.Equal(arguments[:min(len(arguments), 2)], routePrefix) || slices.Contains(arguments, "--model")):
		t.Errorf("route-preserving launch lost --resume-route or forwarded a model: %q", arguments)
	case !route && !slices.Equal(arguments[:min(len(arguments), 5)], modeledPrefix):
		t.Errorf("modeled launch lost its explicit model and provider-only auth: %q", arguments)
	}
}

func TestSuccessorCandidateKeepsPredecessorContinuationValidator(t *testing.T) {
	for _, mode := range successorModes {
		for _, route := range []bool{true, false} {
			reference := preAttributionReference(mode, route)
			t.Run(mode+"/"+reference.Version, func(t *testing.T) {
				predecessor := successorRequest(t, mode)
				predecessor.PriorSession = nil
				successor := predecessor
				successor.RequestedConfig.Model = "changed-alias"
				err := validateContinuation(predecessor, task.MetaRecord{Predicate: reference}, successor)
				if route && !errors.Is(err, commonprovider.ErrUnsupportedOption) {
					t.Errorf("route-preserving predecessor model change error=%v, want %v", err, commonprovider.ErrUnsupportedOption)
				}
				if !route && err != nil {
					t.Errorf("modeled predecessor refused its allowed override: %v", err)
				}
			})
		}
	}
}

// TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed pairs each stored
// predicate that is not an exact v4 reference with the reconstruction of the
// same record: finalized under every capability proof, the successor must
// produce the same predicate or the same error.
func TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed(t *testing.T) {
	for _, mode := range successorModes {
		altered := preAttributionReference(mode, true)
		altered.SHA256 = strings.Repeat("0", 64)
		cases := map[string]task.PredicateRef{
			"current route":     Reference(mode),
			"current modeled":   modeledReference(mode),
			"v3 route":          preCredentialReference(mode, true),
			"v3 modeled":        preCredentialReference(mode, false),
			"v2":                previousReference(mode),
			"v1":                HistoricalReference(mode),
			"altered v4 digest": altered,
			"zero":              {},
		}
		for name, reference := range cases {
			t.Run(mode+"/"+name, func(t *testing.T) {
				request := successorRequest(t, mode)
				requireSuccessorMatchesReconstruction(t, request, successorMeta(request, reference))
			})
		}
	}
}

// capabilityProofs are the launch-help projections a candidate can finalize
// under: both route capabilities and the legacy form without native facts.
var capabilityProofs = []func(*testing.T, commonprovider.ProfileCandidate) json.RawMessage{
	func(t *testing.T, c commonprovider.ProfileCandidate) json.RawMessage { return routeFacts(t, c, true) },
	func(t *testing.T, c commonprovider.ProfileCandidate) json.RawMessage { return routeFacts(t, c, false) },
	legacyFacts,
}

// requireSuccessorMatchesReconstruction requires the successor of meta to fail
// like its reconstruction, or to finalize to the same predicate and arguments
// under every capability proof.
func requireSuccessorMatchesReconstruction(t *testing.T, request task.TaskRecord, meta task.MetaRecord) {
	t.Helper()
	reconstructed, existingErr := PrepareExistingCandidate(request, meta)
	successor, successorErr := PrepareSuccessorCandidate(request, meta)
	if (existingErr == nil) != (successorErr == nil) || (existingErr != nil && existingErr.Error() != successorErr.Error()) {
		t.Fatalf("successor error=%v, reconstruction error=%v", successorErr, existingErr)
	}
	if existingErr != nil {
		return
	}
	for _, facts := range capabilityProofs {
		want, wantErr := reconstructed.Finalize(facts(t, reconstructed), time.Now())
		got, gotErr := successor.Finalize(facts(t, successor), time.Now())
		if (wantErr == nil) != (gotErr == nil) || !got.Plan.Predicate.Equal(want.Plan.Predicate) || !slices.Equal(got.Plan.Arguments, want.Plan.Arguments) {
			t.Errorf("successor finalized %+v err=%v, reconstruction %+v err=%v", got.Plan.Predicate, gotErr, want.Plan.Predicate, wantErr)
		}
	}
}

// TestSuccessorTargetsClassifyCredentialFailure keeps the credential wrapper on
// every predicate a v4 continuation can use: the frozen source and its current
// target both turn an empty exit-77 stream into authentication-unavailable.
func TestSuccessorTargetsClassifyCredentialFailure(t *testing.T) {
	registered := Registration().Interpreters
	for _, mode := range successorModes {
		for _, route := range []bool{true, false} {
			target, _ := currentOutputReference(mode, preAttributionReference(mode, route))
			for _, reference := range []task.PredicateRef{preAttributionReference(mode, route), target} {
				t.Run(mode+"/"+reference.Version, func(t *testing.T) {
					index := slices.IndexFunc(registered, func(i predicate.Interpreter) bool { return i.Reference().Equal(reference) })
					if index < 0 {
						t.Fatalf("%+v is not registered", reference)
					}
					input, raw := credentialInput(t, reference, 77, "", "private native diagnostic")
					result, err := registered[index].Evaluate(input, raw, io.Discard)
					if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalAuthenticationUnavailable {
						t.Fatalf("credential failure = %+v, %v", result, err)
					}
				})
			}
		}
	}
}

func TestSuccessorCandidateRequiresRecordedEnvironment(t *testing.T) {
	request := successorRequest(t, config.ModeReadOnly)
	if _, err := PrepareSuccessorCandidate(request, task.MetaRecord{Predicate: preAttributionReference(config.ModeReadOnly, true)}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unrecorded environment error=%v, want %v", err, task.ErrIdentityMismatch)
	}
}

func TestSuccessorCandidateDoesNotModifyThePredecessorRecord(t *testing.T) {
	request := successorRequest(t, config.ModeWorkspaceWrite)
	meta := successorMeta(request, preAttributionReference(config.ModeWorkspaceWrite, false))
	before := meta
	before.Environment = slices.Clone(meta.Environment)
	if _, err := PrepareSuccessorCandidate(request, meta); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, meta) {
		t.Errorf("predecessor record changed:\nbefore=%+v\nafter=%+v", before, meta)
	}
}
