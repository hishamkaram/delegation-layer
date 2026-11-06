package control

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The pure generation decision over privateGenerationFacts. The facts are
// loaded once under the caller's generation lease; these methods read them
// only and perform no I/O.
type privateGenerationPhase uint8

const (
	privateGenerationUnavailable privateGenerationPhase = iota
	privateGenerationObservable
	privateGenerationComplete
)

func (facts privateGenerationFacts) publishedReady(binding task.SupervisorRef) (bool, error) {
	if !facts.settled || facts.transitionErr != nil {
		return false, facts.transitionErr
	}
	if facts.identityErr != nil || !facts.identityFound {
		return false, facts.identityErr
	}
	if !privateDaemonIdentityMatchesBinding(facts.identity, binding) {
		return false, nil
	}
	return facts.publishedFound && task.SameSupervisorIdentity(facts.published, binding), facts.publishedErr
}

// runtimeRefused is an early positive-mismatch observation, not admission
// authority. Required read faults survive even when a missing or unsettled
// dependency prevents publishedReady from reaching them. Alias/migration
// policy remains with its existing retirement and submission owners.
func (facts privateGenerationFacts) runtimeRefused(runtime task.SupervisorRef) (bool, error) {
	if err := errors.Join(facts.publishedErr, facts.identityErr, facts.transitionErr, facts.identityPublicationReadErr); err != nil {
		return false, err
	}
	ready, err := facts.publishedReady(facts.published)
	if err != nil {
		return false, err
	}
	if ready {
		return !task.SameSupervisorIdentity(facts.published, runtime), nil
	}
	return facts.settled && facts.identityFound && facts.publishedFound, nil
}

func (facts privateGenerationFacts) complete(binding task.SupervisorRef, allowRecoveryAlias bool) (bool, error) {
	if facts.migrationErr != nil || facts.migrationPresent {
		return false, facts.migrationErr
	}
	if !allowRecoveryAlias && (facts.aliasErr != nil || facts.aliasPresent) {
		return false, facts.aliasErr
	}
	return facts.publishedReady(binding)
}

// privateGenerationPurpose names the consumer of a generation decision. Every
// purpose shares the same publication, generation, journal and alias fences;
// only the alias match differs.
type privateGenerationPurpose uint8

const (
	// privateGenerationGeneral serves probes, successor resolution, migration
	// and alias retirement: the alias must hold the saved config's raw bytes.
	privateGenerationGeneral privateGenerationPurpose = iota
	// privateGenerationTaskRead serves only the read of saved task rows under
	// the generation lease. An alias whose own bytes resolve to the saved
	// effective config is equivalent for that read. It grants no submission,
	// repair, cleanup or retirement authority.
	privateGenerationTaskRead
)

func (facts privateGenerationFacts) phase(binding task.SupervisorRef) (privateGenerationPhase, error) {
	return facts.phaseFor(binding, privateGenerationGeneral)
}

func (facts privateGenerationFacts) phaseFor(binding task.SupervisorRef, purpose privateGenerationPurpose) (privateGenerationPhase, error) {
	if facts.migrationErr != nil {
		return privateGenerationUnavailable, facts.migrationErr
	}
	ready, err := facts.publishedReady(binding)
	if err != nil || !ready || !privateFullGenerationMatches(facts.published, binding) {
		return privateGenerationUnavailable, err
	}
	if facts.migrationPresent && (!privateMigrationPrepared(facts.migration) || !privateDaemonIdentityMatchesBinding(facts.migration.Legacy, binding)) {
		return privateGenerationUnavailable, nil
	}
	aliasMatches, aliasErr := facts.observableAliasMatches(binding, purpose)
	if aliasErr != nil || !aliasMatches {
		return privateGenerationUnavailable, aliasErr
	}
	if facts.migrationPresent || facts.aliasPresent {
		return privateGenerationObservable, nil
	}
	return privateGenerationComplete, nil
}

func (facts privateGenerationFacts) observableAliasMatches(binding task.SupervisorRef, purpose privateGenerationPurpose) (bool, error) {
	if facts.aliasErr != nil || facts.aliasPublicationErr != nil || facts.aliasPublishing {
		return false, errors.Join(facts.aliasErr, facts.aliasPublicationErr)
	}
	if !facts.aliasPresent {
		return true, nil
	}
	if purpose == privateGenerationTaskRead {
		return facts.alias.matches(binding), nil
	}
	return task.SameSupervisorIdentity(facts.alias.record.Supervisor, binding) && facts.alias.record.ConfigSHA256 == binding.ConfigDigest, nil
}
