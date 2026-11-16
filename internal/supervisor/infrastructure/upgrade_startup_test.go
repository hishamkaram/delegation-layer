package infrastructure

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestUpgradePreflightBirthHasNoOperationSubjects(t *testing.T) {
	initial := InitialOperation{Kind: InitialUpgradePreflight}
	if err := initial.Validate(originTestRootID); err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(initial)
	if err != nil || !bytes.Equal(data, []byte("{\"kind\":\"upgrade-preflight\"}\n")) {
		t.Fatalf("passive birth descriptor changed its independent golden: %q %v", data, err)
	}
	source, _ := savedBindingSourceFixture(t, SavedBindingTaskMetadata)
	initial.Source = source
	if err := initial.Validate(originTestRootID); err == nil {
		t.Fatal("preflight accepted a saved-task subject")
	}
	initial.Source = SavedBindingSource{}
	initial.RestartFrom = StartupReference{Sequence: 1, Epoch: originTestRootID, IntentSHA256: ownerWireDigest}
	if err := initial.Validate(originTestRootID); err == nil {
		t.Fatal("preflight accepted a restart subject")
	}
	for _, data := range []string{`{"kind":"upgrade-preflight","action":"upgrade-prepare"}`, `{"kind":"upgrade-preflight","operation_id":"upgrade-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, `{"kind":"upgrade-preflight","restart_from":{"sequence":0,"epoch":"","intent_sha256":""}}`, `{"kind":"upgrade-preflight","source":{"root_id":"","kind":"","owner_id":"","record_id":"","binding_sha256":""}}`} {
		if err := task.DecodeStrict([]byte(data), &initial); err == nil {
			t.Fatal("preflight accepted concrete upgrade authority at birth")
		}
	}
}

func TestUpgradePreflightBirthRequiresOutsideProducer(t *testing.T) {
	initial := InitialOperation{Kind: InitialUpgradePreflight}
	for _, producer := range []producerKind{producerOutside, producerResident} {
		err := validateRuntimeRestartProducer(initial, producer)
		if producer == producerOutside && err != nil || producer != producerOutside && err == nil {
			t.Fatalf("producer eligibility mismatch: %s %v", producer, err)
		}
	}
	intent := startupIntentFixture(t)
	intent.Inputs.Initial = initial
	if err := intent.Inputs.Validate(); err == nil {
		t.Fatal("preflight startup omitted producer provenance")
	}
	intent.Inputs.RuntimeProducer = producerOutside
	if err := intent.Inputs.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validateUpgradePreflightBirth(intent, nil); err != nil {
		t.Fatal(err)
	}
	origin, err := NewOrigin(intent.Inputs.Root, intent.Inputs.RootID)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := WithOrigin(nil, origin)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateUpgradePreflightBirth(intent, environment); err == nil {
		t.Fatal("preflight birth accepted a resident/provider origin")
	}
}

func TestUpgradePreflightElectionNeverCreatesMissingOwnerLocks(t *testing.T) {
	for _, name := range []string{instanceLeaseName, filepath.Join("infrastructure", ".operation.lock")} {
		t.Run(name, func(t *testing.T) {
			f := producerFixture(t)
			releaseProducerFixtureLease(t, f)
			path := filepath.Join(f.journal.pin.Root(), name)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			before := producerBudgetTree(t, f.journal.pin.Root())
			request := f.request
			request.Initial = InitialOperation{Kind: InitialUpgradePreflight}
			attempt, err := producerInvocation(t).EstablishOutside(t.Context(), f.journal.store, request, f.host)
			ownProducerAttempt(t, attempt)
			if err == nil || !errors.Is(err, os.ErrNotExist) || f.host.calls != 0 {
				t.Fatalf("missing startup lock was repaired: prepare=%d err=%v", f.host.calls, err)
			}
			if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
				t.Fatal("preflight election created or changed files")
			}
		})
	}
}
