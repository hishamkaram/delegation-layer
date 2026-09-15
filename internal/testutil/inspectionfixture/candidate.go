package inspectionfixture

import (
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/phase2cli"
)

type helperOutput struct {
	SchemaVersion int    `json:"schema_version"`
	Sentinel      string `json:"sentinel"`
	Eligible      bool   `json:"eligible"`
}

type fixtureFacts struct {
	Eligible bool `json:"eligible"`
}

type effectiveBinding struct {
	BaseDigest         string          `json:"base_digest"`
	DefinitionSHA256   string          `json:"definition_sha256"`
	HelperConfigSHA256 string          `json:"helper_config_sha256"`
	Facts              json.RawMessage `json:"facts"`
}

// NewDependencies composes the phase-two provider with the native inspection
// candidate. The existing phase2cli profile factory remains the source of the
// ordinary finite provider plan and predicate.
func NewDependencies() app.Dependencies { return NewDependenciesForExecutable("") }

// NewDependenciesForExecutable composes a copied acceptance wrapper. The
// phase2 event recorder is intentionally disabled here: inspection fixture
// wrappers have no recorder lifecycle of their own, and the pueue/execution
// callbacks must not retain an unjoined recorder after this function returns.
func NewDependenciesForExecutable(executable string) app.Dependencies {
	main := phase2cli.NewMain(executable)
	deps := main.Dependencies()
	prepare := deps.PrepareProfile
	inspectionPath := filepath.Join(filepath.Dir(main.ConfigPath()), ConfigName)
	deps.PrepareCandidate = func(request task.TaskRecord) (provider.ProfileCandidate, error) {
		if prepare == nil {
			return provider.ProfileCandidate{}, provider.ErrProfileUnavailable
		}
		profile, err := prepare(request)
		if err != nil {
			return provider.ProfileCandidate{}, err
		}
		return prepareInspectionCandidate(request, profile, inspectionPath)
	}
	deps.SupervisorOptions.Observer = nil
	deps.ExecutionHooks = execution.Hooks{}
	return deps
}

func prepareInspectionCandidate(request task.TaskRecord, profile provider.PreparedProfile, inspectionPath string) (provider.ProfileCandidate, error) {
	if err := profile.Validate(request); err != nil {
		return provider.ProfileCandidate{}, err
	}
	loaded, err := loadConfig(inspectionPath)
	if err != nil {
		return provider.ProfileCandidate{}, err
	}
	helper, err := loadHelperConfig(loaded.Config.HelperConfig)
	if err != nil || helper.SHA256 != loaded.Config.HelperConfigSHA256 {
		return provider.ProfileCandidate{}, ErrInvalidConfig
	}
	sentinel, err := readCanonicalRegular(helper.Config.SentinelPath, maxSentinelBytes, false)
	if err != nil {
		return provider.ProfileCandidate{}, ErrInvalidHelper
	}
	sentinelSHA256 := task.ComputeSHA256(sentinel)
	clear(sentinel)

	definition := provider.InspectionDefinition{
		Revision:         HelperRevision,
		Executable:       loaded.Config.HelperExecutable,
		ExecutableSHA256: loaded.Config.HelperSHA256,
		Arguments:        []string{loaded.Config.HelperConfig},
		Directory:        profile.Plan.Directory,
		Environment:      slices.Clone(loaded.Config.Environment),
		OutputLimit:      inspectionOutputLimit,
	}
	definition.Project = func(native []byte) (json.RawMessage, error) {
		return projectHelper(native, sentinelSHA256, helper.Config.Eligible)
	}
	snapshot, definitionSHA256, err := definition.Snapshot()
	if err != nil {
		return provider.ProfileCandidate{}, err
	}
	base := clonePreparedProfile(profile)
	return provider.ProfileCandidate{
		Directory:     base.Plan.Directory,
		WritableRoots: slices.Clone(base.WritableRoots),
		Inspection:    &snapshot,
		Finalize: func(facts json.RawMessage, _ time.Time) (provider.PreparedProfile, error) {
			canonical, err := canonicalFacts(facts)
			if err != nil {
				return provider.PreparedProfile{}, err
			}
			binding := effectiveBinding{
				BaseDigest:         base.Effective.Digest,
				DefinitionSHA256:   definitionSHA256,
				HelperConfigSHA256: loaded.Config.HelperConfigSHA256,
				Facts:              canonical,
			}
			bindingBytes, err := task.MarshalCanonical(binding)
			if err != nil {
				return provider.PreparedProfile{}, provider.ErrProfileUnavailable
			}
			prepared := clonePreparedProfile(base)
			prepared.Effective.Digest = task.ComputeSHA256(bindingBytes)
			if err = prepared.Validate(request); err != nil {
				return provider.PreparedProfile{}, err
			}
			return prepared, nil
		},
	}, nil
}

func projectHelper(native []byte, sentinelSHA256 string, eligible bool) (json.RawMessage, error) {
	if len(native) == 0 || int64(len(native)) > inspectionOutputLimit {
		return nil, ErrHelperOutput
	}
	var output helperOutput
	if err := task.DecodeStrict(native, &output); err != nil || output.SchemaVersion != SchemaVersion || !eligible || !output.Eligible || task.ComputeSHA256([]byte(output.Sentinel)) != sentinelSHA256 {
		return nil, ErrHelperOutput
	}
	return task.MarshalCanonical(fixtureFacts{Eligible: true})
}

func canonicalFacts(facts []byte) (json.RawMessage, error) {
	if len(facts) == 0 || int64(len(facts)) > 4<<10 {
		return nil, provider.ErrProfileUnavailable
	}
	var decoded fixtureFacts
	if err := task.DecodeStrict(facts, &decoded); err != nil || !decoded.Eligible {
		return nil, provider.ErrProfileUnavailable
	}
	canonical, err := task.MarshalCanonical(decoded)
	if err != nil {
		return nil, provider.ErrProfileUnavailable
	}
	return canonical, nil
}

func clonePreparedProfile(profile provider.PreparedProfile) provider.PreparedProfile {
	clone := profile
	clone.Plan.Arguments = slices.Clone(profile.Plan.Arguments)
	clone.Plan.Environment = slices.Clone(profile.Plan.Environment)
	clone.Plan.InputFiles = slices.Clone(profile.Plan.InputFiles)
	clone.Plan.OutputArtifacts = slices.Clone(profile.Plan.OutputArtifacts)
	clone.WritableRoots = slices.Clone(profile.WritableRoots)
	clone.Effective = task.CloneEffectiveConfig(profile.Effective)
	return clone
}

// Run is the copied delegate wrapper entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	return app.Run(args, stdout, stderr, NewDependencies())
}

// RunRunner is the copied delegate-run wrapper entry point.
func RunRunner(args []string, stdout, stderr io.Writer) int {
	return app.RunRunner(args, stdout, stderr, NewDependencies())
}
