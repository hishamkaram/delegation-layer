package contributorprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

const contributorRuntimeEnvironment = "DELEGATE_CONTRIBUTOR_RUNTIME"

var ErrUnsupportedProfile = errors.New("unsupported-effective-config")

type runtimeIdentity struct {
	Executable string
	Version    string
	SHA256     string
	OS         string
	Arch       string
}

type prepareDependencies struct {
	Identity      runtimeIdentity
	RuntimeDir    string
	Certification certificationRecord
}

// Prepare resolves the certified fixture executable and the explicitly
// configured private runtime directory. It performs no process, supervisor,
// authentication, or native-provider probe.
func Prepare(request task.TaskRecord) (commonprovider.PreparedProfile, error) {
	record, err := embeddedCertification()
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	identity, err := resolveRuntime(record)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	runtimeDir, err := resolveRuntimeDirectory()
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepareWithDependencies(request, prepareDependencies{Identity: identity, RuntimeDir: runtimeDir, Certification: record})
}

func resolveRuntime(record certificationRecord) (runtimeIdentity, error) {
	if err := record.validate(); err != nil {
		return runtimeIdentity{}, err
	}
	wrapper, err := os.Executable()
	if err != nil {
		return runtimeIdentity{}, fmt.Errorf("%w: resolving contributor wrapper: %w", ErrUnsupportedProfile, err)
	}
	wrapper, err = canonicalExecutable(wrapper, "contributor wrapper")
	if err != nil {
		return runtimeIdentity{}, err
	}
	executable := filepath.Join(filepath.Dir(wrapper), "provider")
	identity, err := inspectExecutable(executable)
	if err != nil {
		return runtimeIdentity{}, fmt.Errorf("%w: contributor provider executable: %w", ErrUnsupportedProfile, err)
	}
	if identity.OS != record.OS || identity.Arch != record.Arch {
		return runtimeIdentity{}, fmt.Errorf("%w: certified provider platform is %s/%s, current runtime is %s/%s", ErrUnsupportedProfile, record.OS, record.Arch, identity.OS, identity.Arch)
	}
	if identity.SHA256 != record.RuntimeSHA256 {
		return runtimeIdentity{}, fmt.Errorf("%w: contributor provider executable differs from measured certification", ErrUnsupportedProfile)
	}
	identity.Version = record.ProviderVersion
	return identity, nil
}

func resolveRuntimeDirectory() (string, error) {
	raw := os.Getenv(contributorRuntimeEnvironment)
	if raw == "" {
		return "", fmt.Errorf("%w: %s is required", ErrUnsupportedProfile, contributorRuntimeEnvironment)
	}
	if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", fmt.Errorf("%w: %s must be a clean absolute path", ErrUnsupportedProfile, contributorRuntimeEnvironment)
	}
	if _, err := validateRuntimeDirectory(raw); err != nil {
		return "", err
	}
	return raw, nil
}

func prepareWithDependencies(request task.TaskRecord, dependencies prepareDependencies) (commonprovider.PreparedProfile, error) {
	if err := validatePreparationRequest(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err := dependencies.Certification.validate(); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err := validateIdentity(dependencies.Identity, dependencies.Certification); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	runtimeDir, err := validateRuntimeDirectory(dependencies.RuntimeDir)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if pathsOverlap(request.CanonicalCwd, runtimeDir) {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: contributor runtime must be outside the workspace", ErrUnsupportedProfile)
	}
	arguments := prepareArguments(request)
	inputs, err := configurationInputs(runtimeDir)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	effective, err := effectiveConfiguration(request, runtimeDir, dependencies.Certification)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: dependencies.Identity.Executable, Arguments: arguments, Directory: request.CanonicalCwd,
			Environment: []string{}, Predicate: Reference(), InputFiles: inputs,
			OutputArtifacts: []task.OutputArtifact{{Name: OutputName, ArgumentIndex: 5}}, OutputWriterContract: OutputWriterContract,
		},
		ObservedVersion: dependencies.Identity.Version,
		Effective:       effective,
		Identity:        identityFactory(request.TaskID),
		WritableRoots:   []string{runtimeDir},
	}, nil
}

func validatePreparationRequest(request task.TaskRecord) error {
	if err := validateRequestProfile(request); err != nil {
		return err
	}
	if err := validateBriefLength(request.BriefLength); err != nil {
		return err
	}
	if err := task.ValidateTaskID(request.TaskID); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	if err := validateExistingCanonicalDirectory(request.CanonicalCwd, "workspace"); err != nil {
		return err
	}
	if request.PriorSession != nil {
		return validatePriorSession(*request.PriorSession)
	}
	return nil
}

func validateRequestProfile(request task.TaskRecord) error {
	if request.Provider != Provider || request.Mode != Mode || request.RequestedConfig.Permission != Mode {
		return fmt.Errorf("%w: contributor proof requires the read-only permission profile", ErrUnsupportedProfile)
	}
	if request.RequestedConfig.Model != "" || (request.RequestedConfig.Effort != "" && request.RequestedConfig.Effort != "default") || request.RequestedConfig.NativeTimeout != "" {
		return fmt.Errorf("%w: contributor proof supports continuation only", ErrUnsupportedProfile)
	}
	if request.BudgetNanos <= 0 {
		return fmt.Errorf("%w: task budget must be positive", ErrUnsupportedProfile)
	}
	return nil
}

func validateBriefLength(length int64) error {
	if length <= 0 || length > MaxBriefBytes {
		return fmt.Errorf("%w: brief length must be positive and at most %d bytes", ErrUnsupportedProfile, MaxBriefBytes)
	}
	return nil
}

func validatePriorSession(prior task.PriorSession) error {
	if prior.Provider != Provider || !validSessionID(prior.ConversationID) {
		return fmt.Errorf("%w: invalid continuation session", ErrUnsupportedProfile)
	}
	if err := task.ValidateTaskID(prior.PredecessorTaskID); err != nil {
		return fmt.Errorf("%w: invalid continuation predecessor: %w", ErrUnsupportedProfile, err)
	}
	return nil
}

func validateIdentity(identity runtimeIdentity, record certificationRecord) error {
	if !filepath.IsAbs(identity.Executable) || filepath.Clean(identity.Executable) != identity.Executable || identity.Version != record.ProviderVersion || identity.OS != record.OS || identity.Arch != record.Arch || identity.SHA256 != record.RuntimeSHA256 {
		return fmt.Errorf("%w: executable identity does not match measured certification", ErrUnsupportedProfile)
	}
	if err := task.ValidateSHA256(identity.SHA256); err != nil {
		return fmt.Errorf("%w: executable identity digest: %w", ErrUnsupportedProfile, err)
	}
	return nil
}

func validateRuntimeDirectory(path string) (string, error) {
	if err := validatePrivateCanonicalDirectory(path, "runtime"); err != nil {
		return "", err
	}
	for _, child := range []string{"sessions", "launches", "completions"} {
		if err := validateOptionalPrivateCanonicalDirectory(filepath.Join(path, child), "runtime/"+child); err != nil {
			return "", err
		}
	}
	return path, nil
}

func validatePrivateCanonicalDirectory(path, label string) error {
	if err := validateExistingCanonicalDirectory(path, label); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %s cannot be inspected: %w", ErrUnsupportedProfile, label, err)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: %s must have private 0700 permissions", ErrUnsupportedProfile, label)
	}
	return nil
}

func validateOptionalPrivateCanonicalDirectory(path, label string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s cannot be inspected: %w", ErrUnsupportedProfile, label, err)
	}
	return validatePrivateCanonicalDirectory(path, label)
}

func validateExistingCanonicalDirectory(path, label string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%w: %s must be a clean absolute path", ErrUnsupportedProfile, label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %s must exist: %w", ErrUnsupportedProfile, label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %s must be a canonical directory", ErrUnsupportedProfile, label)
	}
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path {
		return fmt.Errorf("%w: %s must be canonical", ErrUnsupportedProfile, label)
	}
	return nil
}

func prepareArguments(request task.TaskRecord) []string {
	sessionID := "session-" + request.TaskID
	arguments := []string{"--runtime-config", "", "--tools-config", "", "--output", "", "--task-id", request.TaskID, "--session-id", sessionID}
	if request.PriorSession == nil {
		return arguments
	}
	sessionID = request.PriorSession.ConversationID
	arguments[9] = sessionID
	return append(arguments, "--resume")
}

func configurationInputs(runtimeDir string) ([]task.InputFile, error) {
	runtimeData, err := task.MarshalCanonical(protocol.RuntimeConfig{SchemaVersion: protocol.SchemaVersion, RuntimeDir: runtimeDir})
	if err != nil {
		return nil, err
	}
	toolsData, err := task.MarshalCanonical(protocol.ToolsConfig{SchemaVersion: protocol.SchemaVersion, AllowedTools: []string{}, WorkspaceWrite: false})
	if err != nil {
		return nil, err
	}
	return []task.InputFile{
		{Name: RuntimeInputName, ArgumentIndex: 1, Content: string(runtimeData)},
		{Name: ToolsInputName, ArgumentIndex: 3, Content: string(toolsData)},
	}, nil
}

func effectiveConfiguration(request task.TaskRecord, runtimeDir string, record certificationRecord) (task.EffectiveConfig, error) {
	policy := &task.PolicyDetails{ProfileRevision: record.ProfileRevision, RuntimeSHA256: record.RuntimeSHA256, Workspace: request.CanonicalCwd, WritableRoots: []string{runtimeDir}}
	effective := task.EffectiveConfig{Containment: Mode, Approval: "never", Policy: policy}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	effective.Digest = task.ComputeSHA256(encoded)
	if err := task.ValidateEffectiveConfig(effective); err != nil {
		return task.EffectiveConfig{}, err
	}
	return effective, nil
}

func canonicalExecutable(path, label string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%w: %s must be a clean absolute path", ErrUnsupportedProfile, label)
	}
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path {
		return "", fmt.Errorf("%w: %s must be canonical", ErrUnsupportedProfile, label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("%w: %s cannot be inspected: %w", ErrUnsupportedProfile, label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: %s must be an executable regular file", ErrUnsupportedProfile, label)
	}
	return path, nil
}

func inspectExecutable(path string) (identity runtimeIdentity, resultErr error) {
	path, err := canonicalExecutable(path, "provider executable")
	if err != nil {
		return runtimeIdentity{}, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return runtimeIdentity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return runtimeIdentity{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return runtimeIdentity{}, err
	}
	if !os.SameFile(before, opened) {
		return runtimeIdentity{}, errors.New("provider executable changed before hashing")
	}
	hasher := sha256.New()
	if _, copyErr := io.Copy(hasher, file); copyErr != nil {
		return runtimeIdentity{}, copyErr
	}
	after, err := os.Lstat(path)
	if err != nil {
		return runtimeIdentity{}, err
	}
	if !os.SameFile(after, opened) || after.Size() != opened.Size() || after.ModTime() != opened.ModTime() {
		return runtimeIdentity{}, errors.New("provider executable changed during hashing")
	}
	return runtimeIdentity{Executable: path, SHA256: hex.EncodeToString(hasher.Sum(nil)), OS: runtime.GOOS, Arch: runtime.GOARCH}, nil
}

func pathsOverlap(left, right string) bool {
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
