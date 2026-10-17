package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestExplicitResolutionMustMatchChildEnvironment(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	resolution, err := CurrentResolutionContext()
	if err != nil {
		t.Fatal(err)
	}
	changedHome := filepath.Join(t.TempDir(), "different-home")
	environment := append(append([]string{}, fake.environment...), "HOME="+changedHome)
	if _, bindErr := Bind(context.Background(), fake.executable, fake.configPath, Options{Environment: environment, Resolution: &resolution}); !errors.Is(bindErr, ErrConfiguration) {
		t.Fatalf("accepted resolution for different child environment: %v", bindErr)
	}
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{Environment: fake.environment, Resolution: &resolution})
	if err != nil {
		t.Fatal(err)
	}
	if client.Binding().ResolvedConfigSHA256 == "" {
		t.Fatal("matching explicit environment lost resolved binding")
	}
}

func TestFreshBindingDetectsConfigDrift(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	data, err := os.ReadFile(fake.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if unmarshalErr := json.Unmarshal(data, &document); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	document["client"] = map[string]any{"dark_mode": true}
	changed, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake.configPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := fake.client.Reconcile(context.Background(), identity); !errors.Is(err, ErrBinding) {
		t.Fatalf("config drift was accepted: %v", err)
	}
}

func TestFreshBindingDetectsExecutableDrift(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	file, err := os.OpenFile(fake.executable, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.client.Reconcile(context.Background(), testIdentity()); !errors.Is(err, ErrBinding) {
		t.Fatalf("executable drift was accepted: %v", err)
	}
}

func TestFreshBindingDetectsResolutionDrift(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	if err := os.WriteFile(fake.configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	bound, err := Bind(context.Background(), fake.executable, fake.configPath, Options{Environment: fake.environment})
	if err != nil {
		t.Fatal(err)
	}
	changed := append(append([]string{}, fake.environment...), "HOME="+filepath.Join(t.TempDir(), "different-home"))
	client, err := NewClient(bound.Binding(), Options{Environment: changed, ObservationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("saved supervisor resolution was not reused after ambient drift: %v", err)
	}
	if client.Binding().ResolutionOS == "" {
		t.Fatal("admission did not persist supervisor resolution")
	}
}

func TestSavedBindingDoesNotPinWorkingDirectory(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	saved := fake.client.Binding()
	saved.ResolutionCwd = filepath.Join(t.TempDir(), "removed-before-recovery")
	client, err := NewClient(saved, Options{Environment: fake.environment, ObservationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	removed := t.TempDir()
	t.Cleanup(func() {
		if chdirErr := os.Chdir(current); chdirErr != nil {
			t.Errorf("restore working directory: %v", chdirErr)
		}
	})
	if err = os.Chdir(removed); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	command, err := client.prepareCommand("status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Dir(client.RuntimeBinding().ConfigPath); command.Dir != want {
		t.Fatalf("saved supervisor command directory=%q, want config directory %q", command.Dir, want)
	}
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("saved supervisor binding depended on a removed working directory: %v", err)
	}
}

func TestSavedBindingPreservesExplicitEmptyEnvironment(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	client, err := NewClient(fake.client.Binding(), Options{Environment: []string{}, ObservationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	command, err := client.prepareCommand("status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if command.Env == nil || len(command.Env) != 0 {
		t.Fatalf("saved binding changed an explicit empty environment: %#v", command.Env)
	}
}

func testIdentity() Identity {
	return Identity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64)}
}

func TestNewClientRequiresEverySavedBindingComponent(t *testing.T) {
	base := validBindingForTest()
	cases := map[string]func(*task.SupervisorRef){
		"client executable": func(ref *task.SupervisorRef) { ref.ClientExecutable = "" },
		"client digest":     func(ref *task.SupervisorRef) { ref.ClientSHA256 = "" },
		"resolved digest":   func(ref *task.SupervisorRef) { ref.ResolvedConfigSHA256 = "" },
		"config path":       func(ref *task.SupervisorRef) { ref.ConfigPath = "" },
		"config digest":     func(ref *task.SupervisorRef) { ref.ConfigDigest = "" },
		"endpoint":          func(ref *task.SupervisorRef) { ref.Endpoint = "" },
		"version":           func(ref *task.SupervisorRef) { ref.ObservedVersion = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ref := base
			mutate(&ref)
			if _, err := NewClient(ref, Options{}); err == nil {
				t.Fatal("incomplete binding accepted")
			}
		})
	}
}

func TestOpenRegularRefusesRacedPipeAndFinalSymlink(t *testing.T) {
	base := t.TempDir()
	fifo := filepath.Join(base, "config.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := readRegular(fifo, MaxControlBytes)
		finished <- err
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, ErrConfiguration) {
			t.Fatalf("FIFO refusal lost configuration error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("FIFO read blocked instead of refusing")
	}
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(link, MaxControlBytes); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("final symlink was followed: %v", err)
	}
}
func boolPointer(value bool) *bool { return &value }
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
