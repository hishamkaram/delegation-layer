package task

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeInputFilesSortsAndCopiesDeclarations(t *testing.T) {
	files := []InputFile{
		{Name: "z-profile.json", ArgumentIndex: 3, Content: "z"},
		{Name: "a-profile.json", ArgumentIndex: 1, Content: "a"},
	}
	normalized, err := NormalizeInputFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	if normalized[0].Name != "a-profile.json" || normalized[1].Name != "z-profile.json" {
		t.Fatalf("input files were not sorted: %+v", normalized)
	}
	normalized[0].Content = "x"
	if files[1].Content != "a" {
		t.Fatal("input declaration was not copied")
	}
	if CompareInputFiles(files, normalized) {
		t.Fatal("comparison ignored declaration order")
	}
}

func TestNormalizeInputFilesRejectsMalformedDeclarations(t *testing.T) {
	cases := []struct {
		name  string
		files []InputFile
	}{
		{name: "unsafe name", files: []InputFile{{Name: "../profile", ArgumentIndex: 0, Content: "x"}}},
		{name: "uppercase name", files: []InputFile{{Name: "Profile.json", ArgumentIndex: 0, Content: "x"}}},
		{name: "reserved name", files: []InputFile{{Name: "stdout", ArgumentIndex: 0, Content: "x"}}},
		{name: "negative index", files: []InputFile{{Name: "profile.json", ArgumentIndex: -1, Content: "x"}}},
		{name: "invalid utf8", files: []InputFile{{Name: "profile.json", ArgumentIndex: 0, Content: string([]byte{0xff})}}},
		{name: "duplicate name", files: []InputFile{{Name: "profile.json", ArgumentIndex: 0, Content: "a"}, {Name: "profile.json", ArgumentIndex: 1, Content: "b"}}},
		{name: "aggregate limit", files: []InputFile{{Name: "profile.json", ArgumentIndex: 0, Content: strings.Repeat("x", MaxInputBytes)}, {Name: "mcp.json", ArgumentIndex: 1, Content: "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeInputFiles(tc.files); err == nil {
				t.Fatal("malformed input declaration was accepted")
			}
		})
	}
	tooMany := make([]InputFile, MaxInputFiles+1)
	for index := range tooMany {
		tooMany[index] = InputFile{Name: "file-" + string(rune('a'+index)) + ".json", ArgumentIndex: index, Content: "x"}
	}
	if _, err := NormalizeInputFiles(tooMany); err == nil {
		t.Fatal("too many input files were accepted")
	}
}

func TestNormalizeOutputArtifactsAndWriterContract(t *testing.T) {
	artifacts := []OutputArtifact{
		{Name: "z-last-message.txt", ArgumentIndex: 4},
		{Name: "a-answer.txt", ArgumentIndex: 2},
	}
	normalized, err := NormalizeOutputArtifacts(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if normalized[0].Name != "a-answer.txt" || normalized[1].Name != "z-last-message.txt" {
		t.Fatalf("output artifacts were not sorted: %+v", normalized)
	}
	if err = ValidateOutputArtifacts(artifacts, OutputWriterProcessExitEOF); err != nil {
		t.Fatalf("valid writer contract rejected: %v", err)
	}
	if err = ValidateOutputArtifacts(nil, OutputWriterProcessExitEOF); err == nil {
		t.Fatal("writer contract without an artifact was accepted")
	}
	if err = ValidateOutputArtifacts(artifacts, "snapshot-only-v1"); err == nil {
		t.Fatal("unsupported writer contract was accepted")
	}
	if err = ValidateOutputArtifacts(artifacts, ""); err == nil {
		t.Fatal("artifacts without a writer contract were accepted")
	}
}

func TestArtifactNamesReserveCoreStagingNamespace(t *testing.T) {
	for _, name := range []string{"stage.0123456789abcdef0123456789abcdef.tmp", "stage.profile.json"} {
		if _, err := NormalizeInputFiles([]InputFile{{Name: name, Content: "config"}}); err == nil {
			t.Fatalf("scavenger namespace accepted for input: %s", name)
		}
		if _, err := NormalizeOutputArtifacts([]OutputArtifact{{Name: name}}); err == nil {
			t.Fatalf("scavenger namespace accepted for output: %s", name)
		}
	}
}

func TestValidateInputOutputBindingsRejectsNameAndSlotCollisions(t *testing.T) {
	nameCollision := []InputFile{{Name: "answer.txt", ArgumentIndex: 0, Content: "x"}}
	if err := ValidateInputOutputBindings(nameCollision, []OutputArtifact{{Name: "answer.txt", ArgumentIndex: 1}}); err == nil {
		t.Fatal("input/output name collision was accepted")
	}
	indexCollision := []InputFile{{Name: "profile.json", ArgumentIndex: 0, Content: "x"}}
	if err := ValidateInputOutputBindings(indexCollision, []OutputArtifact{{Name: "answer.txt", ArgumentIndex: 0}}); err == nil {
		t.Fatal("input/output argv collision was accepted")
	}
}

func TestMetaRecordValidatesAndRoundTripsDeclarations(t *testing.T) {
	meta := fixtureMeta(t)
	meta.InputFiles = []InputFile{{Name: "empty-mcp.json", ArgumentIndex: 1, Content: `{"mcpServers":{}}`}}
	meta.OutputArtifacts = []OutputArtifact{{Name: "last-message.txt", ArgumentIndex: 3}}
	meta.OutputWriterContract = OutputWriterProcessExitEOF
	if err := ValidateMetaRecord(&meta); err != nil {
		t.Fatal(err)
	}
	data, err := MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	var decoded MetaRecord
	if err = DecodeStrict(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !CompareInputFiles(meta.InputFiles, decoded.InputFiles) || !CompareOutputArtifacts(meta.OutputArtifacts, decoded.OutputArtifacts) || decoded.OutputWriterContract != OutputWriterProcessExitEOF {
		t.Fatalf("declarations did not round-trip: %+v", decoded)
	}
}

func TestMetaRecordRequiresCanonicalDeclarationOrder(t *testing.T) {
	meta := fixtureMeta(t)
	meta.InputFiles = []InputFile{
		{Name: "z.json", ArgumentIndex: 1, Content: "z"},
		{Name: "a.json", ArgumentIndex: 2, Content: "a"},
	}
	if err := ValidateMetaRecord(&meta); err == nil {
		t.Fatal("unsorted input declarations were accepted")
	}
	meta.InputFiles = nil
	meta.OutputArtifacts = []OutputArtifact{{Name: "z.txt", ArgumentIndex: 1}, {Name: "a.txt", ArgumentIndex: 2}}
	meta.OutputWriterContract = OutputWriterProcessExitEOF
	if err := ValidateMetaRecord(&meta); err == nil {
		t.Fatal("unsorted output declarations were accepted")
	}
}

func TestLegacyMetaAndManifestBytesRemainTwoStreamOnly(t *testing.T) {
	meta := fixtureMeta(t)
	data, err := MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"input_files"`, `"output_artifacts"`, `"output_writer_contract"`} {
		if bytes.Contains(data, []byte(field)) {
			t.Fatalf("legacy metadata unexpectedly contains %s: %s", field, data)
		}
	}
	manifest := []RawManifestEntry{
		{Path: "raw/stderr", Size: 0, SHA256: ComputeSHA256(nil)},
		{Path: "raw/stdout", Size: 0, SHA256: ComputeSHA256(nil)},
	}
	manifestBytes, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateRawManifest(manifest, ComputeSHA256(manifestBytes)); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRawManifestAllowsSafeDeclaredShapeAndRejectsUnsafeExtras(t *testing.T) {
	manifest := []RawManifestEntry{
		{Path: "raw/answer.txt", Size: 0, SHA256: ComputeSHA256(nil)},
		{Path: "raw/stderr", Size: 0, SHA256: ComputeSHA256(nil)},
		{Path: "raw/stdout", Size: 0, SHA256: ComputeSHA256(nil)},
	}
	data, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateRawManifest(manifest, ComputeSHA256(data)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"raw/../escape", "raw/nested/file", "raw/.hidden", "raw/stage.0123456789abcdef0123456789abcdef.tmp"} {
		bad := append([]RawManifestEntry(nil), manifest...)
		bad[0].Path = path
		badData, marshalErr := MarshalCanonical(bad)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err = ValidateRawManifest(bad, ComputeSHA256(badData)); !errors.Is(err, ErrEvidenceFault) {
			t.Fatalf("unsafe path %q accepted: %v", path, err)
		}
	}
	missing := []RawManifestEntry{manifest[0], manifest[2]}
	missingData, err := MarshalCanonical(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateRawManifest(missing, ComputeSHA256(missingData)); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("manifest without stderr accepted: %v", err)
	}
}

func TestRawManifestArtifactCountBoundary(t *testing.T) {
	for _, count := range []int{MaxOutputArtifacts, MaxOutputArtifacts + 1} {
		manifest := make([]RawManifestEntry, 0, count+2)
		for index := range count {
			manifest = append(manifest, RawManifestEntry{Path: "raw/file-" + string(rune('a'+index)), SHA256: ComputeSHA256(nil)})
		}
		manifest = append(manifest, RawManifestEntry{Path: "raw/stderr", SHA256: ComputeSHA256(nil)}, RawManifestEntry{Path: "raw/stdout", SHA256: ComputeSHA256(nil)})
		data, err := MarshalCanonical(manifest)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateRawManifest(manifest, ComputeSHA256(data))
		if count == MaxOutputArtifacts && err != nil {
			t.Fatalf("valid maximum manifest rejected: %v", err)
		}
		if count > MaxOutputArtifacts && !errors.Is(err, ErrEvidenceFault) {
			t.Fatalf("oversized manifest accepted: %v", err)
		}
	}
}
