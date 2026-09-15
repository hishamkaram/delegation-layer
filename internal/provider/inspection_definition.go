package provider

import (
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// MaxInspectionOutput bounds each private native stream and HTTP response.
const MaxInspectionOutput int64 = 1 << 20

// Snapshot validates and copies the serializable effect description. The
// digest never evaluates callbacks or includes their transient credentials.
// The worker executable digest separately pins callback implementations.
func (d InspectionDefinition) Snapshot() (InspectionDefinition, string, error) {
	d.Arguments = slices.Clone(d.Arguments)
	d.Environment = slices.Clone(d.Environment)
	if d.Runtime != nil {
		runtime := *d.Runtime
		runtime.Environment = slices.Clone(runtime.Environment)
		runtime.HelpArgs = slices.Clone(runtime.HelpArgs)
		runtime.RequiredFlags = slices.Clone(runtime.RequiredFlags)
		d.Runtime = &runtime
	}
	if d.Remote != nil {
		remote := *d.Remote
		remote.Headers = maps.Clone(remote.Headers)
		d.Remote = &remote
	}
	if err := d.validate(); err != nil {
		return InspectionDefinition{}, "", err
	}
	data, err := task.MarshalCanonical(d)
	if err != nil {
		return InspectionDefinition{}, "", fmt.Errorf("%w: invalid inspection description", ErrProfileUnavailable)
	}
	if len(data) > task.MaxControlRecordSize {
		return InspectionDefinition{}, "", fmt.Errorf("%w: inspection description exceeds record limit", ErrProfileUnavailable)
	}
	return d, task.ComputeSHA256(data), nil
}

func (d InspectionDefinition) validate() error {
	if err := validateInspectionCommand(d); err != nil {
		return err
	}
	if err := validateInspectionEnvironment(d.Environment); err != nil {
		return err
	}
	if err := validateInspectionArguments(d.Arguments); err != nil {
		return err
	}
	if d.Runtime != nil {
		if err := validateRuntimeProbe(*d.Runtime); err != nil {
			return err
		}
	}
	return validateInspectionProjector(d)
}

func validateInspectionCommand(d InspectionDefinition) error {
	if !inspectionText(d.Revision) || strings.TrimSpace(d.Revision) == "" ||
		d.OutputLimit <= 0 || d.OutputLimit > MaxInspectionOutput ||
		!inspectionPath(d.Executable) || !inspectionPath(d.Directory) ||
		task.ValidateSHA256(d.ExecutableSHA256) != nil || d.Environment == nil {
		return fmt.Errorf("%w: invalid inspection command binding", ErrProfileUnavailable)
	}
	return nil
}

func validateInspectionArguments(arguments []string) error {
	for _, argument := range arguments {
		if !inspectionText(argument) {
			return fmt.Errorf("%w: invalid inspection argument", ErrProfileUnavailable)
		}
	}
	return nil
}

func validateInspectionProjector(d InspectionDefinition) error {
	native := len(d.Arguments) > 0 || d.Project != nil || d.Remote != nil
	if !native {
		if d.Runtime == nil {
			return fmt.Errorf("%w: missing inspection projector", ErrProfileUnavailable)
		}
		if d.Executable != d.Runtime.Executable || d.ExecutableSHA256 != d.Runtime.ExecutableSHA256 ||
			d.Directory != d.Runtime.Directory || !slices.Equal(d.Environment, d.Runtime.Environment) {
			return fmt.Errorf("%w: runtime-only inspection binding is inconsistent", ErrProfileUnavailable)
		}
		return nil
	}
	if d.Remote != nil {
		if d.Project != nil {
			return fmt.Errorf("%w: ambiguous inspection projector", ErrProfileUnavailable)
		}
		return d.Remote.validate()
	}
	if d.Project == nil {
		return fmt.Errorf("%w: missing inspection projector", ErrProfileUnavailable)
	}
	return nil
}

func validateRuntimeProbe(probe RuntimeProbeDefinition) error {
	if !inspectionPath(probe.Executable) || !inspectionPath(probe.Directory) || task.ValidateSHA256(probe.ExecutableSHA256) != nil || probe.Environment == nil {
		return fmt.Errorf("%w: invalid runtime capability binding", ErrProfileUnavailable)
	}
	if err := validateInspectionEnvironment(probe.Environment); err != nil {
		return err
	}
	for _, arg := range probe.HelpArgs {
		if !inspectionText(arg) || strings.ContainsAny(arg, " \t\r\n") {
			return fmt.Errorf("%w: invalid runtime help argument", ErrProfileUnavailable)
		}
	}
	seen := make(map[string]struct{}, len(probe.RequiredFlags))
	for _, flag := range probe.RequiredFlags {
		if !inspectionText(flag) || !strings.HasPrefix(flag, "-") || strings.ContainsAny(flag, " \t\r\n") {
			return fmt.Errorf("%w: invalid runtime required flag", ErrProfileUnavailable)
		}
		if _, exists := seen[flag]; exists {
			return fmt.Errorf("%w: duplicate runtime required flag", ErrProfileUnavailable)
		}
		seen[flag] = struct{}{}
	}
	return nil
}

func validateInspectionEnvironment(environment []string) error {
	seen := make(map[string]bool, len(environment))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !inspectionText(entry) || seen[key] {
			return fmt.Errorf("%w: invalid inspection environment", ErrProfileUnavailable)
		}
		seen[key] = true
	}
	return nil
}

func (d HTTPInspectionDefinition) validate() error {
	endpoint, err := url.Parse(d.URL)
	if err != nil || !inspectionText(d.URL) || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" || d.Authorization == nil || d.Project == nil {
		return fmt.Errorf("%w: invalid remote inspection binding", ErrProfileUnavailable)
	}
	return validateInspectionHeaders(d.Headers)
}

func validateInspectionHeaders(headers map[string]string) error {
	seen := make(map[string]bool, len(headers))
	for name, value := range headers {
		key := strings.ToLower(name)
		if !inspectionHeaderName(name) || !inspectionText(value) || strings.ContainsAny(value, "\r\n") || seen[key] || key == "authorization" || key == "proxy-authorization" || key == "cookie" {
			return fmt.Errorf("%w: invalid fixed inspection header", ErrProfileUnavailable)
		}
		seen[key] = true
	}
	return nil
}

func inspectionText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func inspectionPath(value string) bool {
	return inspectionText(value) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func inspectionHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r > 127 {
			return false
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}
