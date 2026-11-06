package task

import (
	"strings"
	"testing"
)

// Paths and legacy digests are historical data; no executable is opened.
func installedSupervisorFixture() SupervisorRef {
	return SupervisorRef{
		ExecutableIdentity: ExecutableIdentityInstalledPathV1,
		ClientExecutable:   "/opt/tools/pueue", DaemonExecutable: "/opt/tools/pueued",
		ConfigPath: "/state/.supervisor/pueue.yml", ConfigDigest: strings.Repeat("c", 64),
		ResolvedConfigSHA256: strings.Repeat("d", 64), Endpoint: "unix:/state/.supervisor/socket", ObservedVersion: "4.0.4",
	}
}

func legacySupervisorFixture() SupervisorRef {
	ref := installedSupervisorFixture()
	ref.ExecutableIdentity = ""
	ref.ClientSHA256, ref.DaemonSHA256 = strings.Repeat("a", 64), strings.Repeat("b", 64)
	return ref
}

func TestSupervisorExecutableIdentityValidation(t *testing.T) {
	cases := []struct {
		name         string
		ref          SupervisorRef
		valid, fresh bool
	}{
		{"installed", installedSupervisorFixture(), true, true},
		{"legacy", legacySupervisorFixture(), true, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { checkSupervisorExecutableValidation(t, test.ref, test.valid, test.fresh) })
	}
	for _, test := range []struct {
		name   string
		mutate func(*SupervisorRef)
	}{
		{"unknown", func(r *SupervisorRef) { r.ExecutableIdentity = "future-mode" }},
		{"client digest", func(r *SupervisorRef) { r.ClientSHA256 = strings.Repeat("a", 64) }},
		{"daemon digest", func(r *SupervisorRef) { r.DaemonSHA256 = strings.Repeat("b", 64) }},
		{"missing client", func(r *SupervisorRef) { r.ClientExecutable = "" }},
		{"relative client", func(r *SupervisorRef) { r.ClientExecutable = "relative" }},
		{"unclean daemon", func(r *SupervisorRef) { r.DaemonExecutable = "/opt/../pueued" }},
		{"nul client", func(r *SupervisorRef) { r.ClientExecutable = "/opt/pueue\x00" }},
		{"missing config digest", func(r *SupervisorRef) { r.ConfigDigest = "" }},
		{"missing resolved digest", func(r *SupervisorRef) { r.ResolvedConfigSHA256 = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ref := installedSupervisorFixture()
			test.mutate(&ref)
			checkSupervisorExecutableValidation(t, ref, false, false)
		})
	}
	external := installedSupervisorFixture()
	external.DaemonExecutable = ""
	checkSupervisorExecutableValidation(t, external, true, true)
}

func checkSupervisorExecutableValidation(t *testing.T, ref SupervisorRef, valid, fresh bool) {
	t.Helper()
	if err := ValidateSupervisorRef(ref); (err == nil) != valid {
		t.Fatalf("historical validation valid=%t: %v", valid, err)
	}
	if err := ValidateFreshSupervisorRef(ref); (err == nil) != fresh {
		t.Fatalf("fresh validation valid=%t: %v", fresh, err)
	}
}

func TestLegacySupervisorExecutableValidationRemainsDistinct(t *testing.T) {
	for _, test := range []struct {
		name         string
		mutate       func(*SupervisorRef)
		valid, fresh bool
	}{
		{"missing client digest", func(r *SupervisorRef) { r.ClientSHA256 = "" }, true, false},
		{"missing client", func(r *SupervisorRef) { r.ClientExecutable = "" }, true, false},
		{"malformed client digest", func(r *SupervisorRef) { r.ClientSHA256 = "invalid" }, false, false},
		{"malformed daemon digest", func(r *SupervisorRef) { r.DaemonSHA256 = "invalid" }, false, false},
		{"missing resolved digest", func(r *SupervisorRef) { r.ResolvedConfigSHA256 = "" }, true, false},
		{"historical daemon half pair", func(r *SupervisorRef) { r.DaemonSHA256 = "" }, true, true},
		{"unknown marker", func(r *SupervisorRef) { r.ExecutableIdentity = "future-mode" }, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ref := legacySupervisorFixture()
			test.mutate(&ref)
			checkSupervisorExecutableValidation(t, ref, test.valid, test.fresh)
		})
	}
}

func executableReceiptFixture(ref SupervisorRef) SupervisorReceipt {
	root, id := strings.Repeat("1", 32), strings.Repeat("2", 32)
	return SupervisorReceipt{
		SchemaVersion: SchemaVersion, RootID: root, TaskID: id, SpecSHA256: strings.Repeat("e", 64), MetaSHA256: strings.Repeat("f", 64),
		NumericTaskID: 7, Label: "delegate:" + root + ":" + id,
		ConfigPath: ref.ConfigPath, ConfigDigest: ref.ConfigDigest, Endpoint: ref.Endpoint, ObservedVersion: ref.ObservedVersion,
		ClientExecutable: ref.ClientExecutable, ClientSHA256: ref.ClientSHA256, DaemonExecutable: ref.DaemonExecutable, DaemonSHA256: ref.DaemonSHA256, ExecutableIdentity: ref.ExecutableIdentity,
		ResolvedConfigSHA256: ref.ResolvedConfigSHA256, ResolutionOS: ref.ResolutionOS, ResolutionHome: ref.ResolutionHome,
		ResolutionDataLocal: ref.ResolutionDataLocal, ResolutionConfig: ref.ResolutionConfig, ResolutionRuntime: ref.ResolutionRuntime,
		ResolutionUsername: ref.ResolutionUsername, ResolutionCwd: ref.ResolutionCwd,
	}
}

func TestSupervisorReceiptPreservesExecutableIdentity(t *testing.T) {
	for _, ref := range []SupervisorRef{legacySupervisorFixture(), installedSupervisorFixture()} {
		ref.ResolutionOS, ref.ResolutionHome = "linux", "/home/fixture"
		ref.ResolutionConfig, ref.ResolutionDataLocal = "/home/fixture/.config", "/home/fixture/.local/share"
		ref.ResolutionRuntime, ref.ResolutionCwd, ref.ResolutionUsername = "/run/user/fixture", "/work", "fixture"
		receipt := executableReceiptFixture(ref)
		if projected := receipt.SupervisorRef(); projected != ref {
			t.Fatalf("receipt changed saved supervisor: got=%+v want=%+v", projected, ref)
		}
		if err := ValidateSupervisorReceipt(&receipt); err != nil {
			t.Fatal(err)
		}
		data, err := MarshalCanonical(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var decoded SupervisorReceipt
		if err = DecodeStrict(data, &decoded); err != nil || decoded != receipt {
			t.Fatalf("receipt round trip: %+v %v", decoded, err)
		}
	}
}

func TestExecutableIdentityDoesNotChangeLogicalSupervisorIdentity(t *testing.T) {
	legacy, installed := legacySupervisorFixture(), installedSupervisorFixture()
	if legacy == installed || !SameSupervisorIdentity(legacy, installed) {
		t.Fatal("logical comparison became executable identity or structural comparison lost it")
	}
	changed := installed
	changed.ExecutableIdentity = ""
	if changed == installed {
		t.Fatal("structural comparison ignored the marker")
	}
	changed.Endpoint = "unix:/different/socket"
	if SameSupervisorIdentity(installed, changed) {
		t.Fatal("logical comparison lost endpoint")
	}
}
