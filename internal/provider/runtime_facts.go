package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RuntimeInspectionRevision is the stable common revision for the supervised
// executable capability probe. The definition digest also covers its command,
// environment, and required flags, so this revision is not a CLI release pin.
const RuntimeInspectionRevision = "runtime-capability-v1"

// RuntimeProbeDefinition is the data-only description of a provider CLI
// capability probe. The inspection worker owns every process started from it.
type RuntimeProbeDefinition struct {
	Executable       string   `json:"executable"`
	ExecutableSHA256 string   `json:"executable_sha256"`
	Directory        string   `json:"directory"`
	Environment      []string `json:"environment"`
	HelpArgs         []string `json:"help_args"`
	RequiredFlags    []string `json:"required_flags"`
}

// NewRuntimeInspectionDefinition builds a runtime-only inspection definition
// from static executable discovery. The duplicated top-level executable
// fields preserve the existing inspection binding record; Runtime carries the
// capability-specific command contract.
func NewRuntimeInspectionDefinition(info CLIInfo, directory string, environment []string, capability RuntimeCapability) (InspectionDefinition, error) {
	definition := InspectionDefinition{
		Revision:         RuntimeInspectionRevision,
		Executable:       info.Path,
		ExecutableSHA256: info.SHA256,
		Directory:        directory,
		Environment:      append([]string(nil), environment...),
		OutputLimit:      MaxInspectionOutput,
		Runtime: &RuntimeProbeDefinition{
			Executable:       info.Path,
			ExecutableSHA256: info.SHA256,
			Directory:        directory,
			Environment:      append([]string(nil), environment...),
			HelpArgs:         append([]string(nil), capability.HelpArgs...),
			RequiredFlags:    append([]string(nil), capability.RequiredFlags...),
		},
	}
	if definition.Environment == nil {
		definition.Environment = []string{}
	}
	if definition.Runtime.Environment == nil {
		definition.Runtime.Environment = []string{}
	}
	snapshot, _, err := definition.Snapshot()
	return snapshot, err
}

// RuntimeFacts are the nonsecret observations returned by one supervised
// capability probe. Version is whatever valid text the CLI reported; it is
// never compared with a hardcoded release or binary hash.
type RuntimeFacts struct {
	Executable string `json:"executable"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
}

// InspectionFacts is the common envelope delivered to a candidate finalizer.
// Native-only inspections retain their historical raw projection in Native;
// runtime-enabled inspections carry Runtime and may carry Native as well.
type InspectionFacts struct {
	Runtime *RuntimeFacts
	Native  json.RawMessage
}

// ValidateRuntimeFacts validates a capability observation without accepting
// arbitrary native output or release metadata.
func ValidateRuntimeFacts(facts RuntimeFacts) error {
	if !filepath.IsAbs(facts.Executable) || filepath.Clean(facts.Executable) != facts.Executable {
		return errors.New("runtime fact executable is not a clean absolute path")
	}
	if err := task.ValidateSHA256(facts.SHA256); err != nil {
		return fmt.Errorf("runtime fact executable digest: %w", err)
	}
	if err := validateRuntimeVersion(facts.Version); err != nil {
		return err
	}
	return nil
}

func validateRuntimeVersion(version string) error {
	if strings.TrimSpace(version) == "" || !utf8.ValidString(version) {
		return errors.New("runtime fact version is empty or invalid UTF-8")
	}
	for _, character := range version {
		if character < 0x20 || character == 0x7f {
			return errors.New("runtime fact version contains control characters")
		}
	}
	return nil
}

// EncodeInspectionFacts creates the canonical envelope consumed by provider
// finalizers. Native may be nil for a runtime-only provider.
func EncodeInspectionFacts(runtime RuntimeFacts, native json.RawMessage) (json.RawMessage, error) {
	if err := ValidateRuntimeFacts(runtime); err != nil {
		return nil, err
	}
	if native != nil {
		if err := task.ValidateJSONStructure(native); err != nil {
			return nil, err
		}
	}
	fields := map[string]json.RawMessage{}
	runtimeBytes, err := task.MarshalCanonical(runtime)
	if err != nil {
		return nil, err
	}
	fields["runtime"] = bytes.TrimSpace(runtimeBytes)
	if native != nil {
		fields["native"] = bytes.TrimSpace(append([]byte(nil), native...))
	}
	encoded, err := task.MarshalCanonical(fields)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// DecodeInspectionFacts accepts the runtime envelope and preserves legacy
// native-only projections for existing historical interpreters.
func DecodeInspectionFacts(data []byte) (InspectionFacts, error) {
	if err := task.ValidateJSONStructure(data); err != nil {
		return InspectionFacts{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return InspectionFacts{}, errors.New("inspection facts must be an object")
	}
	runtimeRaw, hasRuntime := fields["runtime"]
	if !hasRuntime {
		return InspectionFacts{Native: append(json.RawMessage(nil), data...)}, nil
	}
	for key := range fields {
		if key != "runtime" && key != "native" {
			return InspectionFacts{}, fmt.Errorf("unknown inspection facts field %q", key)
		}
	}
	var runtime RuntimeFacts
	if err := task.DecodeStrict(runtimeRaw, &runtime); err != nil {
		return InspectionFacts{}, err
	}
	if err := ValidateRuntimeFacts(runtime); err != nil {
		return InspectionFacts{}, err
	}
	native := fields["native"]
	if native != nil {
		if err := task.ValidateJSONStructure(native); err != nil {
			return InspectionFacts{}, err
		}
		native = append(json.RawMessage(nil), native...)
	}
	return InspectionFacts{Runtime: &runtime, Native: native}, nil
}
