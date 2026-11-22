package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// PrivateSupervisorDirectoryAbsent is narrower than bootstrap eligibility:
// even an empty/schema-only private directory is present. This passive App
// guard never provisions a lock, repairs state or infers resident absence.
func PrivateSupervisorDirectoryAbsent(ctx context.Context, root taskdir.RootPin) (absent bool, resultErr error) {
	if err := checkPrivateUpgradeRoot(ctx, root); err != nil {
		return false, err
	}
	path := filepath.Join(root.Root(), privateSupervisorDirectory)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, checkPrivateUpgradeRoot(ctx, root)
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, ErrBinding
	}
	directory, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	held, err := directory.Stat()
	if err != nil || !os.SameFile(info, held) {
		return false, errors.Join(ErrBinding, err)
	}
	return false, checkPrivateUpgradeRoot(ctx, root)
}

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
