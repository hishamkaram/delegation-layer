package antigravity

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultProjectBaselineAndUnknownEffects(t *testing.T) {
	const valid = `{"id":"default-cli-project","name":"CLI Project","projectResources":{}}`
	if err := validateDefaultProject([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"other identity":      strings.Replace(valid, "default-cli-project", "other-project", 1),
		"extra folder":        strings.Replace(valid, `"projectResources":{}`, `"projectResources":{"folder":"/outside"}`, 1),
		"permission override": strings.Replace(valid, `"projectResources":{}`, `"projectResources":{},"permissions":{"allow":["write_file(*)"]}`, 1),
		"case alias":          strings.Replace(valid, `"projectResources"`, `"ProjectResources"`, 1),
		"duplicate":           strings.Replace(valid, `"projectResources":{}`, `"projectResources":{},"projectResources":{}`, 1),
		"null resources":      strings.Replace(valid, `"projectResources":{}`, `"projectResources":null`, 1),
		"array resources":     strings.Replace(valid, `"projectResources":{}`, `"projectResources":[]`, 1),
		"null name":           strings.Replace(valid, `"CLI Project"`, `null`, 1),
		"second object":       valid + `{}`,
		"invalid utf8":        strings.Replace(valid, "CLI Project", string([]byte{0xff}), 1),
		"null":                `null`,
		"array":               `[]`,
		"missing":             `{}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateDefaultProject([]byte(input)); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("unsupported project accepted: %v", err)
			}
		})
	}
}
