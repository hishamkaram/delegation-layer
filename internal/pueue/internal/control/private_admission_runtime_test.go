package control

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func privateAdmissionRuntimeFacts(runtime task.SupervisorRef) privateGenerationFacts {
	return privateGenerationFacts{published: runtime, publishedFound: true, identity: privateDaemonIdentityForBinding(runtime), identityFound: true, settled: true}
}

func TestPrivateAdmissionRuntimeRefusalUsesCanonicalReadyPredicate(t *testing.T) {
	runtime := privateAdmissionRuntimeBinding("/state")
	for _, row := range []struct {
		name    string
		change  func(*privateGenerationFacts, *task.SupervisorRef)
		refused bool
	}{
		{"exact", func(*privateGenerationFacts, *task.SupervisorRef) {}, false},
		{"client prime", func(_ *privateGenerationFacts, r *task.SupervisorRef) {
			r.ClientSHA256 = task.ComputeSHA256([]byte("client prime"))
		}, false},
		{"different client path", func(_ *privateGenerationFacts, r *task.SupervisorRef) { r.ClientExecutable = "/different/pueue" }, false},
		{"exact alias", func(f *privateGenerationFacts, _ *task.SupervisorRef) { f.aliasPresent = true }, false},
		{"migration present", func(f *privateGenerationFacts, _ *task.SupervisorRef) { f.migrationPresent = true }, false},
		{"daemon path", func(_ *privateGenerationFacts, r *task.SupervisorRef) { r.DaemonExecutable = "/different/pueued" }, true},
		{"daemon digest", func(_ *privateGenerationFacts, r *task.SupervisorRef) {
			r.DaemonSHA256 = task.ComputeSHA256([]byte("different daemon"))
		}, true},
		{"logical identity", func(_ *privateGenerationFacts, r *task.SupervisorRef) {
			r.ConfigDigest = task.ComputeSHA256([]byte("different config"))
		}, true},
		{"unsettled", func(f *privateGenerationFacts, r *task.SupervisorRef) {
			f.settled = false
			r.DaemonExecutable = "/different/pueued"
		}, false},
		{"missing publication", func(f *privateGenerationFacts, r *task.SupervisorRef) {
			f.publishedFound = false
			r.DaemonExecutable = "/different/pueued"
		}, false},
		{"missing identity", func(f *privateGenerationFacts, r *task.SupervisorRef) {
			f.identityFound = false
			r.DaemonExecutable = "/different/pueued"
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			facts, selected := privateAdmissionRuntimeFacts(runtime), runtime
			row.change(&facts, &selected)
			refused, err := facts.runtimeRefused(selected)
			if err != nil || refused != row.refused {
				t.Fatalf("canonical runtime refusal=%t err=%v, want %t", refused, err, row.refused)
			}
		})
	}
}

func TestPrivateAdmissionRuntimeRequiredFaultsSurviveUnsettledOrMissingFacts(t *testing.T) {
	runtime := privateAdmissionRuntimeBinding("/state")
	publication, identity, transition, rawPublication := errors.New("publication read"), errors.New("identity read"), errors.New("bootstrap read"), errors.New("identity publication close")
	for _, missing := range []string{"none", "unsettled", "publication", "identity"} {
		facts := privateAdmissionRuntimeFacts(runtime)
		facts.publishedErr, facts.identityErr, facts.transitionErr, facts.identityPublicationReadErr = publication, identity, transition, rawPublication
		switch missing {
		case "unsettled":
			facts.settled = false
		case "publication":
			facts.publishedFound = false
		case "identity":
			facts.identityFound = false
		}
		refused, err := facts.runtimeRefused(runtime)
		if refused || !errors.Is(err, publication) || !errors.Is(err, identity) || !errors.Is(err, transition) || !errors.Is(err, rawPublication) {
			t.Fatalf("required fault graph lost for %s: %v", missing, err)
		}
	}
}

func TestPrivateAdmissionRuntimeLeavesAliasAndMigrationErrorsToExistingOwners(t *testing.T) {
	runtime := privateAdmissionRuntimeBinding("/state")
	facts := privateAdmissionRuntimeFacts(runtime)
	facts.aliasErr, facts.aliasPublicationErr, facts.migrationErr = errors.New("alias read"), errors.New("alias publication read"), errors.New("migration read")
	refused, err := facts.runtimeRefused(runtime)
	if err != nil || refused {
		t.Fatalf("preflight adopted retirement policy: refused=%t err=%v", refused, err)
	}
}
