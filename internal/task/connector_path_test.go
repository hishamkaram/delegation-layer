package task

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootPathProjectFeatureRequiresAllConnectorFeatures(t *testing.T) {
	features := []string{NamespaceAdmissionFeature, NamespaceMutationFeature, WorkspaceConstrainedConnectorFeature, PathProjectConnectorFeature}
	root := RootRecord{SchemaVersion: SchemaVersion, RootID: strings.Repeat("a", 32), CreatedAt: "2026-09-13T00:00:00Z", RequiredFeatures: features}
	if err := ValidateRootRecord(&root); err != nil {
		t.Fatal("path-capable writer rejected current feature:", err)
	}
	for missing := range 3 {
		changed := root
		changed.RequiredFeatures = append(append([]string{}, features[:missing]...), features[missing+1:]...)
		if err := ValidateRootRecord(&changed); !errors.Is(err, ErrUnsupportedSchema) {
			t.Fatalf("missing %s accepted: %v", features[missing], err)
		}
	}
}

func TestDirectoryBindingSeparatesPathAuthorityFromPhysicalObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project")
	binding := DirectoryIdentity{Kind: PathDirectoryBinding, Path: path}
	first := DirectoryIdentity{Path: path, Device: 7, Inode: 19}
	replacement := DirectoryIdentity{Path: path, Device: 11, Inode: 23}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	if !binding.MatchesObservation(first) || !binding.MatchesObservation(replacement) {
		t.Fatal("path binding treated a historical inode as authority")
	}
	if first.MatchesObservation(replacement) || binding.MatchesObservation(binding) {
		t.Fatal("physical authority or observation typing was weakened")
	}
	wrongPath := replacement
	wrongPath.Path = filepath.Join(t.TempDir(), "other")
	if binding.MatchesObservation(wrongPath) {
		t.Fatal("path binding accepted another canonical path")
	}
	for _, invalid := range []DirectoryIdentity{
		{Kind: PathDirectoryBinding, Path: path, Device: 1},
		{Kind: PathDirectoryBinding, Path: path, Inode: 1},
		{Kind: "unknown", Path: path},
		{Path: path},
		{Kind: PathDirectoryBinding, Path: "relative"},
		{Kind: PathDirectoryBinding, Path: filepath.Join(t.TempDir(), string([]byte{0xff}))},
	} {
		if err := invalid.Validate(); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("invalid binding %+v accepted: %v", invalid, err)
		}
	}
	legacyBytes := DirectoryIdentity{Path: filepath.Join(t.TempDir(), string([]byte{0xff})), Device: 7, Inode: 19}
	if err := legacyBytes.Validate(); err != nil {
		t.Fatal("new path wire restriction changed legacy physical validity:", err)
	}
}

func TestLocalPathConnectorContextAllowsNoGatewayOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project")
	record := TaskRecord{CanonicalCwd: path, ConnectorContext: &ConnectorContext{RootToken: strings.Repeat("a", 64), Project: DirectoryIdentity{Kind: PathDirectoryBinding, Path: path}}}
	if err := ValidateConnectorBinding(&record); err != nil {
		t.Fatal("local path context rejected:", err)
	}
	record.Origin = &GatewayOrigin{}
	if err := ValidateConnectorBinding(&record); err == nil {
		t.Fatal("path binding bypassed validation of a supplied origin")
	}
	record.Origin = nil
	record.ConnectorContext.Project = DirectoryIdentity{Path: path, Device: 7, Inode: 19}
	if err := ValidateConnectorBinding(&record); err == nil {
		t.Fatal("legacy context no longer requires Gateway origin")
	}
}
