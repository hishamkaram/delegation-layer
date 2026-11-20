package task

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectorRootTokenOriginalCanonicalPreimage(t *testing.T) {
	// This literal specifies the original field order and final newline, without
	// calling the production serializer to build the expected preimage.
	preimage := []byte("{\"version\":1,\"root_id\":\"00112233445566778899aabbccddeeff\",\"device\":7,\"inode\":11}\n")
	digest := sha256.Sum256(preimage)
	actual, err := ConnectorRootToken("00112233445566778899aabbccddeeff", RootContextCoordinates{Device: 7, Inode: 11})
	if err != nil {
		t.Fatal(err)
	}
	if expected := hex.EncodeToString(digest[:]); actual != expected {
		t.Fatalf("v1 token changed: got %s, want %s", actual, expected)
	}
}

func TestRootContextBindingRequiresFeatureAndValidPhysicalOrigin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	valid := RootContextBinding{Version: 1, Root: root, Origin: RootContextCoordinates{Device: 7, Inode: 11}, Accepted: RootContextCoordinates{Device: 8, Inode: 11}, Challenge: strings.Repeat("a", 64), ConfirmedAt: "2026-11-20T00:00:00Z"}
	base := RootRecord{SchemaVersion: SchemaVersion, RootID: strings.Repeat("1", 32), CreatedAt: valid.ConfirmedAt}
	if err := ValidateRootRecord(&base); err != nil {
		t.Fatal(err)
	}
	base.ContextBinding, base.RequiredFeatures = &valid, []string{RootContextBindingFeature}
	if err := ValidateRootRecord(&base); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing-feature", "missing-binding", "version", "replacement-inode", "zero-inode", "unclean-path", "challenge", "timestamp"} {
		t.Run(name, func(t *testing.T) {
			binding, record := valid, base
			record.ContextBinding = &binding
			switch name {
			case "missing-feature":
				record.RequiredFeatures = nil
			case "missing-binding":
				record.ContextBinding = nil
			case "version":
				binding.Version = 2
			case "replacement-inode":
				binding.Accepted.Inode++
			case "zero-inode":
				binding.Origin.Inode, binding.Accepted.Inode = 0, 0
			case "unclean-path":
				binding.Root = root + string(filepath.Separator) + "."
			case "challenge":
				binding.Challenge = "bad"
			case "timestamp":
				binding.ConfirmedAt = "bad"
			}
			if err := ValidateRootRecord(&record); err == nil {
				t.Fatalf("accepted invalid binding: %s", name)
			}
		})
	}
}
