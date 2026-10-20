package control

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// rewritePreparedRecoveryAlias replaces the alias bytes and records their new
// digest, so the alias stays record-consistent. Equivalence of the effective
// config, not who wrote the bytes, decides the task read.
func rewritePreparedRecoveryAlias(t *testing.T, fixture preparedPrivateObservationFixture, edit func([]byte) ([]byte, error)) {
	t.Helper()
	withPreparedPrivateFaultLock(t, fixture, func() {
		record, found, err := readPrivateRecoveryBinding(fixture.stateRoot)
		if err != nil || !found {
			t.Fatalf("read recovery record: found=%t err=%v", found, err)
		}
		original := readPrivateSupervisorFile(t, record.ConfigPath)
		data, err := edit(original)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(data, original) {
			t.Fatal("alias edit left the raw bytes unchanged")
		}
		record.ConfigSHA256 = task.ComputeSHA256(data)
		writePrivateSuccessorTestBytes(t, record.ConfigPath, data)
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(fixture.stateRoot), record)
	})
}

func reserializedPreparedAlias(fixture preparedPrivateObservationFixture) func([]byte) ([]byte, error) {
	return func(data []byte) ([]byte, error) {
		return configWithSocketPath(data, strings.TrimPrefix(fixture.legacy.Binding().Endpoint, "unix:"))
	}
}

func commentedPreparedAlias(data []byte) ([]byte, error) {
	return append(data, []byte("\n# equivalent comment-only alias\n")...), nil
}

// readPreparedAliasTaskRow reads the saved task row through a client bound to
// the retained alias, the runtime a recovered saved-task reader uses.
func readPreparedAliasTaskRow(t *testing.T, fixture preparedPrivateObservationFixture, identity Identity) (Observation, error) {
	t.Helper()
	binding := fixture.legacy.Binding()
	client, _, found, err := recoverPrivateRecoveryBinding(t.Context(), fixture.stateRoot, binding, binding.ClientExecutable, binding.DaemonExecutable, fixture.legacy.options)
	if err != nil || !found || client == nil {
		t.Fatalf("recover prepared alias: found=%t client=%v err=%v", found, client, err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if client.RuntimeBinding().ConfigPath != privateRecoveryConfigPath(fixture.stateRoot) {
		t.Fatal("reader did not use the retained recovery alias")
	}
	return client.ReconcileStatus(t.Context(), identity)
}

// An alias whose own bytes differ but resolve to the saved effective config
// serves the read-only task row, through both the saved and the alias runtime.
// The same state stays refused by every strict reader and by the mutator.
func TestPrivateTaskReadAcceptsEquivalentRecoveryAlias(t *testing.T) {
	cases := map[string]func(preparedPrivateObservationFixture) func([]byte) ([]byte, error){
		"reserialized": reserializedPreparedAlias,
		"comment-only": func(preparedPrivateObservationFixture) func([]byte) ([]byte, error) { return commentedPreparedAlias },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			fixture, identity := newPreparedPrivateTaskStatus(t)
			rewritePreparedRecoveryAlias(t, fixture, edit(fixture))
			for runtime, read := range map[string]func() (Observation, error){
				"saved": func() (Observation, error) { return fixture.legacy.ReconcileStatus(t.Context(), identity) },
				"alias": func() (Observation, error) { return readPreparedAliasTaskRow(t, fixture, identity) },
			} {
				if observation, err := read(); err != nil || !observation.Matched || observation.NumericTaskID != 42 {
					t.Fatalf("%s runtime lost the equivalent alias task row: %+v err=%v", runtime, observation, err)
				}
			}
			assertPreparedPrivateObservationIncomplete(t, fixture)
			assertBusyPreparedPrivateObservation(t, fixture)
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			for _, reader := range preparedPrivateObservationReaders() {
				if binding, err := readPreparedPrivateGeneration(t, fixture, fixture.legacy, reader); err == nil {
					t.Errorf("strict %s reader accepted a changed alias: %+v", reader.name, binding)
				}
			}
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
		})
	}
}

type inequivalentAliasCase struct {
	fault  func(*testing.T, preparedPrivateObservationFixture)
	causes []error
	detail string
}

// An alias that is not the saved effective config, or any changed fence,
// refuses the task row with its own cause and changes no state.
func TestPrivateTaskReadRefusesInequivalentRecoveryAlias(t *testing.T) {
	cases := map[string]inequivalentAliasCase{
		"effective config changed": {causes: []error{ErrPrivateSupervisorMigrationRequired}, fault: func(t *testing.T, fixture preparedPrivateObservationFixture) {
			rewritePreparedRecoveryAlias(t, fixture, configWithFileEditMode)
		}},
		"endpoint changed": {causes: []error{ErrBinding}, detail: "socket path changed", fault: func(t *testing.T, fixture preparedPrivateObservationFixture) {
			rewritePreparedRecoveryAlias(t, fixture, func(data []byte) ([]byte, error) {
				return configWithSocketPath(data, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "other.sock"))
			})
		}},
		"corrupt bytes": {causes: []error{ErrBinding}, detail: "legacy recovery config changed", fault: func(t *testing.T, fixture preparedPrivateObservationFixture) {
			withPreparedPrivateFaultLock(t, fixture, func() { faultPreparedPrivateRecoveryMetadata(t, fixture.stateRoot, "corrupt-recovery-alias") })
		}},
		"foreign identity": {causes: []error{ErrPrivateSupervisorMigrationRequired}, fault: func(t *testing.T, fixture preparedPrivateObservationFixture) {
			withPreparedPrivateFaultLock(t, fixture, func() { faultPreparedPrivateRecoveryMetadata(t, fixture.stateRoot, "foreign-recovery") })
		}},
		"pending alias publication": {causes: []error{ErrPrivateSupervisorMigrationRequired}, fault: func(t *testing.T, fixture preparedPrivateObservationFixture) {
			rewritePreparedRecoveryAlias(t, fixture, commentedPreparedAlias)
			withPreparedPrivateFaultLock(t, fixture, func() {
				writePrivateSuccessorTestBytes(t, privateRecoveryPendingRecordPath(fixture.stateRoot), []byte("{}"))
			})
		}},
		"shutdown requested": {causes: []error{ErrPrivateSupervisorMigrationRequired}, fault: func(t *testing.T, fixture preparedPrivateObservationFixture) {
			rewritePreparedRecoveryAlias(t, fixture, commentedPreparedAlias)
			faultPreparedPrivateObservation(t, fixture, "shutdown-requested")
		}},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture, identity := newPreparedPrivateTaskStatus(t)
			testCase.fault(t, fixture)
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			observation, err := fixture.legacy.ReconcileStatus(t.Context(), identity)
			if err == nil || observation.Matched {
				t.Fatalf("inequivalent alias served the task row: %+v err=%v", observation, err)
			}
			for _, cause := range testCase.causes {
				if !errors.Is(err, cause) {
					t.Fatalf("refusal = %v lost its cause %v", err, cause)
				}
			}
			if testCase.detail != "" && !strings.Contains(err.Error(), testCase.detail) {
				t.Fatalf("refusal = %v, want the %q cause", err, testCase.detail)
			}
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
			assertRefusedPrivateStatusCache(t, fixture.stateRoot, nil)
		})
	}
}

// homeRelativePreparedAlias rewrites every shared path of the alias as a
// home-relative "~/" path from savedHome. Under the saved context it names the
// same private paths; under any other home it names different ones.
func homeRelativePreparedAlias(t *testing.T, savedHome string) func([]byte) ([]byte, error) {
	return func(data []byte) ([]byte, error) {
		lines := strings.Split(string(data), "\n")
		rewritten := 0
		for index, line := range lines {
			key, value, found := strings.Cut(line, ": ")
			switch strings.TrimSpace(key) {
			case "pueue_directory", "runtime_directory", "alias_file", "unix_socket_path", "pid_path", "shared_secret_path":
			default:
				continue
			}
			absolute, err := strconv.Unquote(value)
			if !found || err != nil || !filepath.IsAbs(absolute) {
				return nil, fmt.Errorf("alias %s is not a quoted absolute path: %q", key, value)
			}
			relative, err := filepath.Rel(savedHome, absolute)
			if err != nil {
				return nil, err
			}
			lines[index] = key + ": " + strconv.Quote("~/"+relative)
			rewritten++
		}
		if rewritten != 6 {
			t.Fatalf("rewrote %d alias paths, want all 6 shared paths", rewritten)
		}
		return []byte(strings.Join(lines, "\n")), nil
	}
}

// resolvePreparedAliasDigest resolves the alias's own bytes with the canonical
// parser and resolver under resolution.
func resolvePreparedAliasDigest(t *testing.T, root string, resolution ResolutionContext) (ResolvedConfig, string) {
	t.Helper()
	config, err := ParseConfig(readPrivateSupervisorFile(t, privateRecoveryConfigPath(root)))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := resolved.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return resolved, digest
}

// foreignResolution is the saved context moved to another home, with every
// home-derived directory moved with it.
func foreignResolution(t *testing.T, saved ResolutionContext) ResolutionContext {
	t.Helper()
	home := canonicalTemp(t)
	foreign := saved
	foreign.Home = home
	foreign.DataLocalDirectory = filepath.Join(home, ".local", "share")
	foreign.ConfigDirectory = filepath.Join(home, ".config")
	foreign.RuntimeDirectory = filepath.Join(home, "run")
	return foreign
}

// newHomeRelativePreparedAlias prepares an alias whose paths depend on the
// resolver home, and proves its same bytes resolve to the saved effective
// config under the recorded saved context but differently under another. The
// fixture's saved home is the canonical test temporary directory, so the
// private paths sit below it.
func newHomeRelativePreparedAlias(t *testing.T) (preparedPrivateObservationFixture, Identity, ResolutionContext) {
	t.Helper()
	t.Setenv("HOME", filepath.Dir(canonicalTemp(t)))
	fixture, identity := newPreparedPrivateTaskStatus(t)
	saved := fixture.legacy.Binding()
	resolution, present, err := resolutionFromBinding(saved)
	if err != nil || !present {
		t.Fatalf("saved binding has no recorded resolution: present=%t err=%v", present, err)
	}
	rewritePreparedRecoveryAlias(t, fixture, homeRelativePreparedAlias(t, resolution.Home))
	foreign := foreignResolution(t, resolution)
	savedResolved, savedDigest := resolvePreparedAliasDigest(t, fixture.stateRoot, resolution)
	foreignResolved, foreignDigest := resolvePreparedAliasDigest(t, fixture.stateRoot, foreign)
	if savedDigest != saved.ResolvedConfigSHA256 || foreignDigest == savedDigest || foreignResolved.PueueDirectory == savedResolved.PueueDirectory {
		t.Fatalf("alias bytes are not resolver-sensitive: saved=%s foreign=%s want saved=%s", savedDigest, foreignDigest, saved.ResolvedConfigSHA256)
	}
	return fixture, identity, foreign
}

// savedPreparedClient rebuilds the saved-binding client, as status readers do,
// so it runs under the binding's recorded resolution context.
func savedPreparedClient(t *testing.T, fixture preparedPrivateObservationFixture) *Client {
	t.Helper()
	client, err := NewClient(fixture.legacy.Binding(), fixture.legacy.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

// A resolver-sensitive alias stays readable after the ambient HOME and XDG
// directories change: the task read resolves it under the recorded saved
// context, through both the saved and the recovered-alias runtime.
func TestPrivateTaskReadResolvesAliasUnderSavedContext(t *testing.T) {
	fixture, identity, foreign := newHomeRelativePreparedAlias(t)
	t.Setenv("HOME", foreign.Home)
	t.Setenv("XDG_DATA_HOME", foreign.DataLocalDirectory)
	t.Setenv("XDG_CONFIG_HOME", foreign.ConfigDirectory)
	t.Setenv("XDG_RUNTIME_DIR", foreign.RuntimeDirectory)
	saved := savedPreparedClient(t, fixture)
	for runtime, read := range map[string]func() (Observation, error){
		"saved": func() (Observation, error) { return saved.ReconcileStatus(t.Context(), identity) },
		"alias": func() (Observation, error) { return readPreparedAliasTaskRow(t, fixture, identity) },
	} {
		if observation, err := read(); err != nil || !observation.Matched || observation.NumericTaskID != 42 {
			t.Fatalf("%s runtime lost the alias under ambient drift: %+v err=%v", runtime, observation, err)
		}
	}
	assertPreparedPrivateObservationIncomplete(t, fixture)
	assertBusyPreparedPrivateObservation(t, fixture)
}

// The same alias bytes under a foreign recorded context, with the logical
// identity unchanged, resolve outside the private base. The task read refuses
// with the private-path isolation fence and changes no state or cache.
func TestPrivateTaskReadRefusesAliasUnderForeignRecordedContext(t *testing.T) {
	fixture, identity, foreign := newHomeRelativePreparedAlias(t)
	withPreparedPrivateFaultLock(t, fixture, func() {
		record, found, err := readPrivateRecoveryBinding(fixture.stateRoot)
		if err != nil || !found {
			t.Fatalf("read recovery record: found=%t err=%v", found, err)
		}
		before := record.Supervisor
		setBindingResolution(&record.Supervisor, foreign)
		if !task.SameSupervisorIdentity(record.Supervisor, before) {
			t.Fatal("foreign context changed the alias's logical identity")
		}
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(fixture.stateRoot), record)
	})
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	observation, err := savedPreparedClient(t, fixture).ReconcileStatus(t.Context(), identity)
	if err == nil || observation.Matched || !errors.Is(err, ErrConfiguration) || !strings.Contains(err.Error(), "private path escapes base") {
		t.Fatalf("foreign recorded context served the task row: %+v err=%v", observation, err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
	assertRefusedPrivateStatusCache(t, fixture.stateRoot, nil)
}
