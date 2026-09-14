package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const defaultProjectID = "default-cli-project"

// validateDefaultProject accepts the observed sparse default CLI project only.
// A project with resources or permission overrides needs its own verified
// schema/profile; it cannot inherit this baseline's certification. The implicit
// cwd behavior of this exact empty-resource shape still needs the native gate.
func validateDefaultProject(data []byte) error {
	if !utf8.Valid(data) || task.ValidateJSONStructure(data) != nil {
		return projectPolicyError("invalid project JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 3 {
		return projectPolicyError("unsupported project fields")
	}
	for _, key := range []string{"id", "name", "projectResources"} {
		if _, ok := fields[key]; !ok {
			return projectPolicyError("missing exact project field")
		}
	}
	var id, name string
	if err := json.Unmarshal(fields["id"], &id); err != nil || id != defaultProjectID {
		return projectPolicyError("unsupported project identity")
	}
	if err := json.Unmarshal(fields["name"], &name); err != nil || strings.TrimSpace(name) == "" || strings.ContainsRune(name, '\x00') {
		return projectPolicyError("invalid project display name")
	}
	return validateEmptyProjectResources(fields["projectResources"])
}

func validateEmptyProjectResources(data []byte) error {
	resources := bytes.TrimSpace(data)
	var members map[string]json.RawMessage
	if len(resources) == 0 || resources[0] != '{' || json.Unmarshal(resources, &members) != nil || len(members) != 0 {
		return projectPolicyError("project resources or extra folders are unsupported")
	}
	return nil
}

func projectPolicyError(reason string) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedProfile, reason)
}
