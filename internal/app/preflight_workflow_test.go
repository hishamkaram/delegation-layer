package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type preflightFixture struct {
	root, cwd                   string
	prepareCalls, finalizeCalls int
	prepareFault, finalizeFault error
}

func (fixture *preflightFixture) candidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	fixture.prepareCalls++
	if fixture.prepareFault != nil {
		return commonprovider.ProfileCandidate{}, fixture.prepareFault
	}
	return commonprovider.ProfileCandidate{Directory: request.CanonicalCwd, Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
		fixture.finalizeCalls++
		if fixture.finalizeFault != nil {
			return commonprovider.PreparedProfile{}, fixture.finalizeFault
		}
		ref := preflightFixturePredicate(request.Provider)
		return commonprovider.PreparedProfile{Plan: execution.Plan{Executable: "/fixture/provider", Directory: request.CanonicalCwd, Predicate: ref}, ObservedVersion: "fixture", Effective: task.EffectiveConfig{Containment: "fixture", Approval: "never", Digest: task.ComputeSHA256([]byte("fixture"))}}, nil
	}}, nil
}

func preflightFixturePredicate(id string) task.PredicateRef {
	return task.PredicateRef{Adapter: id, Mode: config.ModeReadOnly, Version: "1", SHA256: task.ComputeSHA256([]byte(id))}
}

func preflightFixtureCatalog(t *testing.T, prepare commonprovider.PrepareCandidate) commonprovider.Catalog {
	t.Helper()
	const id = "fixture:preflight"
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{
		Description: commonprovider.Description{ID: id, SupportedModes: []string{config.ModeReadOnly}, SupportedOptions: []string{commonprovider.OptionContinuation}, Continuation: commonprovider.ContinuationNative, ReadOnly: commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned}, Runtime: commonprovider.RuntimeCapability{RequiredFlags: []string{"--fixture"}}, Discoverable: true},
		Prepare:     prepare, Interpreters: []predicate.Interpreter{autoTestInterpreter{reference: preflightFixturePredicate(id)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestPreflightWorkflowKeepsStaticEvidenceAndNeverCreatesAuthority(t *testing.T) {
	for _, test := range []struct {
		name, provider, model             string
		prepareFault, finalizeFault       error
		status, reason                    string
		code, prepareCalls, finalizeCalls int
	}{
		{name: "ready", status: preflightStatusReady, reason: "static_admission_ready", prepareCalls: 1, finalizeCalls: 1},
		{name: "blocked candidate", prepareFault: commonprovider.ErrAuthenticationBlocked, status: preflightStatusBlocked, reason: "static_admission_rejected", code: 2, prepareCalls: 1},
		{name: "blocked finalizer", finalizeFault: commonprovider.ErrAuthenticationBlocked, status: preflightStatusBlocked, reason: "static_admission_rejected", code: 2, prepareCalls: 1, finalizeCalls: 1},
		{name: "unsupported finalizer", finalizeFault: errors.New("fixture unavailable"), status: preflightStatusUnsupported, reason: "static_admission_rejected", code: 2, prepareCalls: 1, finalizeCalls: 1},
		{name: "unknown provider", provider: "missing:provider", code: 2},
		{name: "unsupported option", model: "unsupported", code: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := preflightFixture{root: filepath.Join(t.TempDir(), "state"), cwd: t.TempDir(), prepareFault: test.prepareFault, finalizeFault: test.finalizeFault}
			catalog := preflightFixtureCatalog(t, fixture.candidate)
			id := test.provider
			if id == "" {
				id = "fixture:preflight"
			}
			result := preflight(Arguments{Root: fixture.root, Cwd: fixture.cwd, Provider: id, Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m", Model: test.model}}, Dependencies{Catalog: catalog})
			if result.code != test.code || result.response.Status != test.status || fixture.prepareCalls != test.prepareCalls || fixture.finalizeCalls != test.finalizeCalls {
				t.Fatalf("static preflight order/result changed: %+v, calls=%d/%d", result, fixture.prepareCalls, fixture.finalizeCalls)
			}
			checkPreflightCapability(t, result.response.Capability, test.status, test.reason)
			if _, err := os.Lstat(fixture.root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preflight created durable state/supervisor authority: %v", err)
			}
		})
	}
}

func checkPreflightCapability(t *testing.T, report *CapabilityReport, status, reason string) {
	t.Helper()

	if reason == "" {
		if report != nil {
			t.Fatal("capability published before catalog request validation")
		}
	} else {
		if report == nil || report.Verification != "preflight" || report.ReasonCode != reason {
			t.Fatalf("wrong static capability: %+v", report)
		}
		wantAuthentication, wantAcceptance := authenticationStatusUnknown, acceptanceStatusNotRun
		if status == preflightStatusBlocked {
			wantAuthentication, wantAcceptance = authenticationStatusBlocked, acceptanceStatusBlocked
		}
		if report.LiveAcceptance.Status != wantAcceptance || report.LiveAcceptance.Authentication != wantAuthentication {
			t.Fatalf("static preflight invented live proof: %+v", report.LiveAcceptance)
		}
	}
}
