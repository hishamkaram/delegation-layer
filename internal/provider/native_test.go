package provider

import (
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// nativeFixtureDigest was produced by NativeEffectiveConfig before the
// revision-parameterized builder existed. Records admitted under the shared
// native revision must keep this digest.
const nativeFixtureDigest = "6e67ff23d73f80ee33a21ad5b8d5ea43a6fbbf087e87ecf7eaf037ecfe52fca5"

func TestNativeEffectiveConfigRejectsInvalidUTF8WorkingDirectory(t *testing.T) {
	request := task.TaskRecord{
		Mode: "workspace-write", CanonicalCwd: "/workspace/" + string([]byte{0xff}),
	}
	effective, err := NativeEffectiveConfig(request, nil, "", "build:auto")
	if err == nil || !strings.Contains(err.Error(), "policy workspace") || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 working directory must produce a workspace validation error: %v", err)
	}
	if effective != (task.EffectiveConfig{}) {
		t.Fatalf("invalid working directory returned a persistable effective configuration: %+v", effective)
	}
}

func TestNativeEffectiveConfigForRevisionKeepsSharedDigest(t *testing.T) {
	request := task.TaskRecord{Mode: "workspace-write", CanonicalCwd: "/workspace"}
	roots := []string{"/workspace/b", "/workspace/a", "/workspace/b"}
	const runtimeSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	shared, err := NativeEffectiveConfig(request, roots, runtimeSHA256, "build:auto")
	if err != nil {
		t.Fatal(err)
	}
	if shared.Digest != nativeFixtureDigest || !HasNativeProfile(task.MetaRecord{EffectiveConfig: shared}) {
		t.Fatalf("shared native config=%+v want digest %s", shared, nativeFixtureDigest)
	}
	explicit, err := NativeEffectiveConfigForRevision(request, roots, runtimeSHA256, "build:auto", NativeProfileRevision)
	if err != nil {
		t.Fatal(err)
	}
	if !task.CompareEffectiveConfigs(shared, explicit) || explicit.Digest != shared.Digest {
		t.Fatalf("explicit shared revision=%+v want %+v", explicit, shared)
	}

	adapter, err := NativeEffectiveConfigForRevision(request, roots, runtimeSHA256, "build:auto", "adapter-native-v2")
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Policy.ProfileRevision != "adapter-native-v2" || adapter.Digest == shared.Digest || HasNativeProfile(task.MetaRecord{EffectiveConfig: adapter}) {
		t.Fatalf("adapter revision config=%+v", adapter)
	}
	if !slices.Equal(adapter.Policy.WritableRoots, []string{"/workspace/a", "/workspace/b"}) {
		t.Fatalf("writable roots=%q", adapter.Policy.WritableRoots)
	}
	if _, err = NativeEffectiveConfigForRevision(request, roots, runtimeSHA256, "build:auto", ""); err == nil {
		t.Fatal("empty profile revision was accepted")
	}
}
