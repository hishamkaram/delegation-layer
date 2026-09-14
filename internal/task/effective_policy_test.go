package task

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

const effectivePolicyTestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validPolicyDetailsForTest() *PolicyDetails {
	return &PolicyDetails{
		ProfileRevision: "agy-baseline-v1",
		RuntimeSHA256:   effectivePolicyTestDigest,
		Workspace:       "/workspace",
		WritableRoots:   []string{"/workspace/cache", "/workspace/tmp"},
		Sources: []PolicySourceDigest{
			{Path: "/config/agy.yaml", Kind: "directory:membership", Present: true, SHA256: effectivePolicyTestDigest},
			{Path: "/config/agy.yaml", Kind: "file:settings", Present: true, SHA256: effectivePolicyTestDigest},
			{Path: "/config/missing.yaml", Kind: "file:settings", Present: false},
		},
	}
}

func TestValidateEffectiveConfigPolicy(t *testing.T) {
	base := EffectiveConfig{Containment: "workspace", Approval: "never", Digest: effectivePolicyTestDigest, Policy: validPolicyDetailsForTest()}
	if err := ValidateEffectiveConfig(base); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(*PolicyDetails)
	}{
		{"profile-revision", func(p *PolicyDetails) { p.ProfileRevision = " " }},
		{"invalid-utf8-profile", func(p *PolicyDetails) { p.ProfileRevision = string([]byte{0xff}) }},
		{"runtime-digest", func(p *PolicyDetails) { p.RuntimeSHA256 = "invalid" }},
		{"relative-workspace", func(p *PolicyDetails) { p.Workspace = "workspace" }},
		{"unclean-workspace", func(p *PolicyDetails) { p.Workspace = "/workspace/../workspace" }},
		{"relative-root", func(p *PolicyDetails) { p.WritableRoots[0] = "relative" }},
		{"unsorted-roots", func(p *PolicyDetails) {
			p.WritableRoots[0], p.WritableRoots[1] = p.WritableRoots[1], p.WritableRoots[0]
		}},
		{"duplicate-root", func(p *PolicyDetails) { p.WritableRoots[1] = p.WritableRoots[0] }},
		{"too-many-roots", func(p *PolicyDetails) { p.WritableRoots = make([]string, MaxPolicyWritableRoots+1) }},
		{"relative-source", func(p *PolicyDetails) { p.Sources[0].Path = "relative" }},
		{"blank-source-kind", func(p *PolicyDetails) { p.Sources[0].Kind = "" }},
		{"invalid-utf8-source-kind", func(p *PolicyDetails) { p.Sources[0].Kind = string([]byte{0xff}) }},
		{"unsorted-sources", func(p *PolicyDetails) { p.Sources[0], p.Sources[1] = p.Sources[1], p.Sources[0] }},
		{"duplicate-source-identity", func(p *PolicyDetails) { p.Sources[1].Kind = p.Sources[0].Kind }},
		{"present-source-digest", func(p *PolicyDetails) { p.Sources[0].SHA256 = "invalid" }},
		{"absent-source-digest", func(p *PolicyDetails) { p.Sources[2].SHA256 = effectivePolicyTestDigest }},
		{"too-many-sources", func(p *PolicyDetails) { p.Sources = make([]PolicySourceDigest, MaxPolicySources+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := *validPolicyDetailsForTest()
			tc.mutate(&policy)
			candidate := base
			candidate.Policy = &policy
			if err := ValidateEffectiveConfig(candidate); err == nil {
				t.Fatal("invalid effective policy accepted")
			}
		})
	}
}

func TestValidateMetaRecordCallsEffectivePolicyValidation(t *testing.T) {
	valid := fixtureMeta(t)
	valid.EffectiveConfig.Policy = validPolicyDetailsForTest()
	if err := ValidateMetaRecord(&valid); err != nil {
		t.Fatal(err)
	}
	valid.EffectiveConfig.Policy.ProfileRevision = ""
	if err := ValidateMetaRecord(&valid); err == nil {
		t.Fatal("invalid effective policy accepted by meta validation")
	}
}

func TestEffectiveConfigPolicyJSONAndLegacyBytes(t *testing.T) {
	legacy := EffectiveConfig{Containment: "workspace", Approval: "never", Digest: effectivePolicyTestDigest}
	encoded, err := MarshalCanonical(legacy)
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte(fmt.Sprintf("{\"containment\":\"workspace\",\"approval\":\"never\",\"digest\":\"%s\"}\n", effectivePolicyTestDigest))
	if !bytes.Equal(encoded, expected) {
		t.Fatalf("legacy effective config encoding changed: %s", encoded)
	}

	withPolicy := legacy
	withPolicy.Policy = validPolicyDetailsForTest()
	encoded, err = MarshalCanonical(withPolicy)
	if err != nil {
		t.Fatal(err)
	}
	var decoded EffectiveConfig
	if err := DecodeStrict(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !CompareEffectiveConfigs(withPolicy, decoded) {
		t.Fatal("policy effective config did not round-trip semantically")
	}
	if !strings.Contains(string(encoded), `"profile_revision":"agy-baseline-v1"`) || !strings.Contains(string(encoded), `"writable_roots"`) {
		t.Fatalf("policy details missing from encoded effective config: %s", encoded)
	}
}

func TestCompareEffectiveConfigsAndClone(t *testing.T) {
	original := EffectiveConfig{Containment: "workspace", Approval: "never", Digest: effectivePolicyTestDigest, Policy: validPolicyDetailsForTest()}
	cloned := CloneEffectiveConfig(original)
	if !CompareEffectiveConfigs(original, cloned) {
		t.Fatal("cloned effective config differs")
	}
	cloned.Policy.WritableRoots[0] = "/workspace/changed"
	cloned.Policy.Sources[0].Path = "/config/changed"
	if original.Policy.WritableRoots[0] == cloned.Policy.WritableRoots[0] || original.Policy.Sources[0].Path == cloned.Policy.Sources[0].Path {
		t.Fatal("policy slices were not deeply cloned")
	}
	if CompareEffectiveConfigs(original, cloned) {
		t.Fatal("changed policy compared equal")
	}

	nilCollections := *validPolicyDetailsForTest()
	nilCollections.WritableRoots = nil
	nilCollections.Sources = nil
	emptyCollections := *validPolicyDetailsForTest()
	emptyCollections.WritableRoots = []string{}
	emptyCollections.Sources = []PolicySourceDigest{}
	left := original
	left.Policy = &nilCollections
	right := original
	right.Policy = &emptyCollections
	if !CompareEffectiveConfigs(left, right) {
		t.Fatal("nil and empty optional policy collections should compare equal")
	}

	withoutPolicy := original
	withoutPolicy.Policy = nil
	if CompareEffectiveConfigs(withoutPolicy, original) {
		t.Fatal("absent and present policy details should not compare equal")
	}
	if !CompareEffectiveConfigs(withoutPolicy, withoutPolicy) {
		t.Fatal("nil policy details should compare equal")
	}
}
