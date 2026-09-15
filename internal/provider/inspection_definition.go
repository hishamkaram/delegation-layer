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
	if !inspectionText(d.Revision) || strings.TrimSpace(d.Revision) == "" ||
		!inspectionPath(d.Executable) || !inspectionPath(d.Directory) ||
		task.ValidateSHA256(d.ExecutableSHA256) != nil ||
		d.Environment == nil || d.OutputLimit <= 0 || d.OutputLimit > MaxInspectionOutput {
		return fmt.Errorf("%w: invalid inspection command binding", ErrProfileUnavailable)
	}
	for _, arg := range d.Arguments {
		if !inspectionText(arg) {
			return fmt.Errorf("%w: invalid inspection argument", ErrProfileUnavailable)
		}
	}
	if err := validateInspectionEnvironment(d.Environment); err != nil {
		return err
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
