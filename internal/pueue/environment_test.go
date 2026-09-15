package pueue

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDefaultEnvironmentKeepsControlValuesAndDropsAmbientState(t *testing.T) {
	control := map[string]string{
		"HOME":                    filepath.Join(t.TempDir(), "home"),
		"PATH":                    "/usr/bin:/bin",
		"USER":                    "fixture-user",
		"LOGNAME":                 "fixture-login",
		"SHELL":                   "/bin/sh",
		"LANG":                    "C",
		"LC_ALL":                  "C.UTF-8",
		"LC_CTYPE":                "C.UTF-8",
		"TZ":                      "UTC",
		"TMPDIR":                  "/tmp",
		"TMP":                     "/tmp",
		"TEMP":                    "/tmp",
		"__CF_USER_TEXT_ENCODING": "0x0:0:0",
	}
	if runtime.GOOS == "linux" {
		control["XDG_DATA_HOME"] = filepath.Join(t.TempDir(), "data")
		control["XDG_CONFIG_HOME"] = filepath.Join(t.TempDir(), "config")
		control["XDG_RUNTIME_DIR"] = filepath.Join(t.TempDir(), "runtime")
	}
	for key, value := range control {
		t.Setenv(key, value)
	}
	const secret = "pueue-client-secret-sentinel"
	t.Setenv("DLP_PUEUE_SECRET_SENTINEL", secret)

	entries := environmentForCommand(nil)
	if entries == nil {
		t.Fatal("nil options environment was left as an ambient-inheritance request")
	}
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !defaultEnvironmentKey(key) {
			t.Fatalf("default environment contains an unapproved entry: %q", entry)
		}
		values[key] = value
	}
	for key, want := range control {
		if got, ok := values[key]; !ok || got != want {
			t.Fatalf("default environment lost control variable %s: got=%q present=%t want=%q", key, got, ok, want)
		}
	}
	if _, present := values["DLP_PUEUE_SECRET_SENTINEL"]; present || strings.Contains(strings.Join(entries, "\x00"), secret) {
		t.Fatal("default environment exposed an unrelated ambient sentinel")
	}
}

func TestCommandUsesBoundedEnvironmentForNilOptions(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	const secret = "pueue-command-secret-sentinel"
	t.Setenv("DLP_PUEUE_SECRET_SENTINEL", secret)

	client := &Client{
		binding: task.SupervisorRef{ClientExecutable: "/fixture/pueue", ConfigPath: "/fixture/pueue.yml"},
		options: Options{ObservationTimeout: time.Second},
	}
	cmd, err := client.prepareCommand("--version")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args, []string{"/fixture/pueue", "-c", "/fixture/pueue.yml", "--version"}) {
		t.Fatalf("wrong prepared command argv: %q", cmd.Args)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != cwd {
		t.Fatalf("wrong prepared command directory: got=%q want=%q", cmd.Dir, cwd)
	}
	environment := make(map[string]string, len(cmd.Env))
	for _, entry := range cmd.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed prepared command environment entry: %q", entry)
		}
		environment[key] = value
	}
	if environment["HOME"] != home || environment["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("prepared command lost required control environment: %v", environment)
	}
	if _, present := environment["DLP_PUEUE_SECRET_SENTINEL"]; present || strings.Contains(strings.Join(cmd.Env, "\x00"), secret) {
		t.Fatal("prepared command received unrelated ambient secret sentinel")
	}
}

func TestPrepareCommandPreservesExplicitEmptyEnvironment(t *testing.T) {
	client := &Client{
		binding: task.SupervisorRef{ClientExecutable: "/fixture/pueue", ConfigPath: "/fixture/pueue.yml"},
		options: Options{Environment: []string{}},
	}
	cmd, err := client.prepareCommand("status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("prepared command changed explicit empty environment: %#v", cmd.Env)
	}
	if !slices.Equal(cmd.Args, []string{"/fixture/pueue", "-c", "/fixture/pueue.yml", "status", "--json"}) {
		t.Fatalf("wrong prepared command argv for explicit empty environment: %q", cmd.Args)
	}
}

func TestEnvironmentForCommandPreservesExplicitValuesAndEmpty(t *testing.T) {
	explicit := []string{"DLP_PUEUE_SECRET_SENTINEL=caller-controlled", "PATH=/explicit"}
	if got := environmentForCommand(explicit); !slices.Equal(got, explicit) {
		t.Fatalf("explicit environment was rewritten: got=%q want=%q", got, explicit)
	}
	got := environmentForCommand([]string{})
	if got == nil || len(got) != 0 {
		t.Fatalf("explicit empty environment changed meaning: %#v", got)
	}
	if got := environmentForCommand(nil); got == nil {
		t.Fatal("nil environment did not resolve to a bounded child environment")
	}
}

func TestDefaultEnvironmentRejectsFreshResolutionDrift(t *testing.T) {
	firstHome := filepath.Join(t.TempDir(), "home-one")
	t.Setenv("HOME", firstHome)
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data-one"))
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config-one"))
		t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime-one"))
	}
	resolution, err := CurrentResolutionContext()
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{options: Options{Resolution: &resolution}}

	t.Setenv("HOME", filepath.Join(t.TempDir(), "home-two"))
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data-two"))
	}
	if _, err := client.resolution(); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("fresh HOME/XDG drift was accepted: %v", err)
	}
}
