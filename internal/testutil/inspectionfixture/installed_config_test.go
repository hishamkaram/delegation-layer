package inspectionfixture

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionFixtureSelectsExecutableMetadataWithoutContentFingerprint(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(dir, "inspection-helper")
	writeFixtureFile(t, helperPath, []byte("#!/bin/sh\nexit 0\n"), 0o111)
	artifacts := filepath.Join(dir, "artifacts")
	if err = os.Mkdir(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "sentinel")
	writeFixtureFile(t, sentinel, []byte("literal finite sentinel"), 0o600)
	helperConfig := filepath.Join(dir, "helper.json")
	helperData := writeFixtureJSON(t, helperConfig, HelperConfig{SchemaVersion: SchemaVersion, ArtifactDir: artifacts, SentinelPath: sentinel, Eligible: true})
	// This fresh configuration deliberately has no helper_sha256 property.
	cfg := struct {
		SchemaVersion      int      `json:"schema_version"`
		HelperExecutable   string   `json:"helper_executable"`
		HelperConfig       string   `json:"helper_config"`
		HelperConfigSHA256 string   `json:"helper_config_sha256"`
		Environment        []string `json:"environment"`
	}{SchemaVersion, helperPath, helperConfig, task.ComputeSHA256(helperData), []string{}}
	path := filepath.Join(dir, ConfigName)
	raw := writeFixtureJSON(t, path, cfg)
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("installed metadata helper config was rejected: %v", err)
	}
	if loaded.Config.HelperExecutable != helperPath || loaded.SHA256 != task.ComputeSHA256(raw) {
		t.Fatal("helper selection or fixture config digest changed")
	}
	request := fixtureRequest(dir)
	candidate, err := prepareInspectionCandidate(request, fixtureProfile(request), path)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Inspection == nil || candidate.Inspection.Executable != helperPath || candidate.Inspection.ExecutableSHA256 != "" {
		t.Fatalf("fresh helper definition has program fingerprint: %+v", candidate.Inspection)
	}
	retired := bytes.TrimSpace(raw)
	retired = append(append([]byte(nil), retired[:len(retired)-1]...), []byte(",\"helper_sha256\":null}\n")...)
	writeFixtureFile(t, path, retired, 0o600)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("retired helper SHA fixture field was accepted")
	}
	writeFixtureFile(t, path, raw, 0o600)
	writeFixtureFile(t, helperConfig, append(helperData, '\n'), 0o600)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("changed helper config bytes escaped the data digest")
	}
}
