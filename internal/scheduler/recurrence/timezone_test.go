package recurrence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTimezoneExplicitAndNativeTZ(t *testing.T) {
	for _, name := range []string{"UTC", "Etc/GMT+5", "America/New_York", "US/Eastern"} {
		got, err := ResolveTimezone(name)
		if err != nil || got != name {
			t.Fatalf("explicit %q = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"Local", "No/Such_Zone", "/etc/localtime", " UTC", "UTC\n"} {
		if _, err := ResolveTimezone(name); !errors.Is(err, ErrInvalidTimezone) {
			t.Fatalf("invalid explicit %q: %v", name, err)
		}
	}
	for _, tz := range []string{"", ":", "UTC", ":UTC", "America/New_York", ":US/Eastern"} {
		t.Setenv("TZ", tz)
		got, err := ResolveTimezone("")
		want := tz
		if len(want) > 0 && want[0] == ':' {
			want = want[1:]
		}
		if want == "" {
			want = "UTC"
		}
		if err != nil || got != want {
			t.Fatalf("TZ=%q = %q, %v; want %q", tz, got, err, want)
		}
	}
	t.Setenv("TZ", "No/Such_Zone")
	if _, err := ResolveTimezone(""); !errors.Is(err, ErrLocalTimezone) {
		t.Fatalf("unresolved native TZ: %v", err)
	}
}

func timezoneFixture(t *testing.T) (string, string) {
	t.Helper()
	data, err := os.ReadFile("/usr/share/zoneinfo/America/New_York")
	if err != nil {
		t.Fatal("native zoneinfo fixture unavailable: ", err)
	}
	root := filepath.Join(t.TempDir(), "zoneinfo")
	path := filepath.Join(root, "America", "New_York")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestLocalTimezonePathIdentityAndAliases(t *testing.T) {
	root, path := timezoneFixture(t)
	alias := filepath.Join(root, "US", "Eastern")
	if err := os.Mkdir(filepath.Dir(alias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "America", "New_York"), alias); err != nil {
		t.Fatal(err)
	}
	localtime := filepath.Join(t.TempDir(), "localtime")
	if err := os.Symlink(alias, localtime); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, want string
	}{{path, "America/New_York"}, {alias, "US/Eastern"}, {localtime, "America/New_York"}} {
		got, err := resolveLocalTimezone(localZoneSource{localtime: test.path, roots: []string{root}})
		if err != nil || got != test.want {
			t.Fatalf("resolve %s = %q, %v; want %q", test.path, got, err, test.want)
		}
	}
	rootAlias := filepath.Join(t.TempDir(), "native root alias")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatal(err)
	}
	got, err := resolveLocalTimezone(localZoneSource{tzPresent: true, tz: ":" + alias, roots: []string{rootAlias}})
	if err != nil || got != "America/New_York" {
		t.Fatalf("physical root alias = %q, %v", got, err)
	}
}

func TestLocalTimezoneRejectsSuffixGuessesCopiesAndEscapes(t *testing.T) {
	root, path := timezoneFixture(t)
	outside := filepath.Join(t.TempDir(), "zoneinfo", "America", "New_York")
	if err := os.MkdirAll(filepath.Dir(outside), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "US", "Eastern")
	if err := os.Mkdir(filepath.Dir(escape), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{outside, escape, filepath.Join(root, "missing"), root} {
		_, err := resolveLocalTimezone(localZoneSource{localtime: candidate, roots: []string{root}})
		if !errors.Is(err, ErrLocalTimezone) {
			t.Fatalf("unidentified path %q: %v", candidate, err)
		}
	}
}
