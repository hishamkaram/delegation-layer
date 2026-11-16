package control

import (
	"errors"
	"os"
	"path/filepath"
)

// PrivateSupervisorInitialBootstrapEligible reports only the absence of any
// predecessor authority. A schema-only ordinary bootstrap intent may resume.
// The existing journal owners retain all mutation and launch authority. A
// stable decision requires the caller's existing generation SH or bootstrap EX;
// this observation neither provisions state nor creates a lease.
func PrivateSupervisorInitialBootstrapEligible(stateRoot string) (bool, error) {
	facts, err := loadPrivateGenerationFacts(stateRoot)
	if err != nil {
		return false, err
	}
	bootstrap, _, bootstrapErr := readPrivateBootstrapRecord(stateRoot)
	_, publishing, publicationErr := readPrivateIdentityPublication(stateRoot)
	_, historyFound, historyErr := loadPrivateMigrationHistory(filepath.Join(stateRoot, privateSupervisorDirectory))
	recoveryPublicationErr := readInitialBootstrapRecoveryPublication(stateRoot)
	if err = errors.Join(facts.publishedErr, facts.identityErr, facts.migrationErr, facts.aliasErr,
		facts.aliasPublicationErr, facts.transitionErr, facts.identityPublicationReadErr,
		bootstrapErr, publicationErr, historyErr, recoveryPublicationErr); err != nil {
		return false, err
	}
	if bootstrap.Restart != nil || bootstrap.RuntimeRestart != nil || bootstrap.DrainV1 != nil {
		return false, nil
	}
	return !facts.publishedFound && !facts.identityFound && !facts.migrationPresent &&
		!facts.aliasPresent && !facts.aliasPublishing && !publishing && !historyFound, nil
}

// The general generation observer detects staged artifacts without decoding
// them. This initial-bootstrap decision also preserves malformed intent errors.
func readInitialBootstrapRecoveryPublication(stateRoot string) error {
	data, err := readRegular(privateRecoveryPendingRecordPath(stateRoot), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	_, err = decodePrivateRecoveryPublication(stateRoot, data)
	return err
}
