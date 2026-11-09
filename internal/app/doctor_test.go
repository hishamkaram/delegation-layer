package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func doctorBundle(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"delegate", "delegate-run", "pueue", "pueued"} {
		// If accidentally executed, this fixture creates a visible sentinel.
		contents := "#!/bin/sh\ntouch '" + filepath.Join(directory, "launched") + "'\nexit 99\n"
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(directory, "delegate")
}

func TestDoctorPassiveObservations(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent root", true: "existing root"}[existing], func(t *testing.T) {
			self := doctorBundle(t)
			root := filepath.Join(t.TempDir(), "state")
			if existing {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			report, code := observeDoctor(Arguments{Root: root}, Dependencies{}, self, nil)
			if code != 0 || report.Installation.Status != "observed" || report.Installation.Compatibility != "not_checked" || report.Supervisor.Liveness != "not_checked" || report.Supervisor.ActiveWork != "unknown" {
				t.Fatalf("unexpected readiness claim: %+v code=%d", report, code)
			}
			checkDoctorRootUnchanged(t, root, report, existing)
			if _, err := os.Lstat(filepath.Join(filepath.Dir(self), "launched")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("doctor launched an installed role: %v", err)
			}
		})
	}
}

func checkDoctorRootUnchanged(t *testing.T, root string, report DoctorReport, existing bool) {
	t.Helper()
	if existing {
		if report.Root.Status != "present" || report.Supervisor.Binding != "binding_absent" {
			t.Fatalf("absent binding misclassified: %+v", report)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("doctor changed root: %v %v", entries, err)
		}
	} else if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor initialized root: %v", err)
	}
}

func TestDoctorIncompleteObservationTakesPrecedenceAndRetainsBlocker(t *testing.T) {
	self := doctorBundle(t)
	if err := os.Remove(filepath.Join(filepath.Dir(self), "pueued")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".supervisor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".supervisor", "supervisor.identity.json"), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, code := observeDoctor(Arguments{Root: root}, Dependencies{}, self, nil)
	if code != 1 || report.Installation.Roles["pueued"].Status != "missing" || len(report.Issues) != 2 {
		t.Fatalf("lost precedence/facts: %+v code=%d", report, code)
	}
}

func TestDoctorProviderStaticAdmissionKeepsAuthenticationUnknown(t *testing.T) {
	for _, fault := range []error{nil, commonprovider.ErrAuthenticationBlocked} {
		fixture := preflightFixture{cwd: t.TempDir(), prepareFault: fault}
		captured := false
		catalog := preflightFixtureCatalog(t, func(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
			captured = len(environment) == 1 && environment[0] == "DOCTOR_SCOPE=selected" && request.RequestedConfig.Permission == config.ModeReadOnly
			return fixture.candidate(request, environment)
		})
		root := filepath.Join(t.TempDir(), "state")
		report, code := observeDoctor(Arguments{Root: root, Cwd: fixture.cwd, Provider: "fixture:preflight", Config: task.TaskConfig{Permission: config.ModeReadOnly}}, Dependencies{Catalog: catalog, Environment: []string{"DOCTOR_SCOPE=selected"}}.normalized(), doctorBundle(t), nil)
		expected := 0
		if fault != nil {
			expected = 2
		}
		if code != expected || !captured || report.Provider.Authentication != "unknown" || report.Provider.Verification != "static_preflight" {
			t.Fatalf("provider report/capture: %+v code=%d captured=%v", report, code, captured)
		}
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created root: %v", err)
		}
	}
}

func TestDoctorArgumentsAndPublicJSON(t *testing.T) {
	for _, args := range [][]string{{"--runner", "/tmp/runner"}, {"--pueue-config=", ""}, {"--provider", "claude:print"}, {"--cwd", "/tmp"}, {"--model="}, {"--effort", "high"}, {"--review-target", "uncommitted"}, {"--permission", "workspace-write"}, {"--budget", "1m"}} {
		if _, err := ParseArguments(append([]string{"doctor"}, args...)); err == nil {
			t.Fatalf("accepted invalid doctor arguments: %v", args)
		}
	}
	parsed, err := ParseArguments([]string{"--root", "/tmp/state", "doctor", "--provider", "codex:review", "--cwd", "/tmp", "--review-target", "uncommitted", "--json"})
	if err != nil || parsed.Config.Permission != config.ModeReadOnly {
		t.Fatalf("valid request: %+v %v", parsed, err)
	}
	var output bytes.Buffer
	code := Run([]string{"doctor", "--root", filepath.Join(t.TempDir(), "state"), "--json"}, &output, &bytes.Buffer{}, Dependencies{Environment: []string{}})
	var report DoctorReport
	if err = json.Unmarshal(output.Bytes(), &report); err != nil || report.Command != "doctor" || report.SchemaVersion != 1 || len(report.Installation.Roles) != 4 || code != 2 {
		t.Fatalf("public JSON: %s code=%d err=%v", output.String(), code, err)
	}
	if code = Run([]string{"doctor", "--json"}, failingDoctorWriter{}, &bytes.Buffer{}, Dependencies{}); code != 1 {
		t.Fatalf("output failure code=%d", code)
	}
}

type failingDoctorWriter struct{}

func (failingDoctorWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestDoctorIncompleteProviderErrorsPreserveEveryCause(t *testing.T) {
	var syntaxValue any
	syntaxErr := json.Unmarshal([]byte("{"), &syntaxValue)
	for _, fault := range []error{
		errors.Join(&os.PathError{Op: "stat", Path: "missing", Err: os.ErrNotExist}, &os.PathError{Op: "open", Path: "private", Err: os.ErrPermission}),
		syntaxErr,
	} {
		fixture := preflightFixture{cwd: t.TempDir(), prepareFault: fault}
		report, code := observeDoctor(Arguments{Root: filepath.Join(t.TempDir(), "state"), Cwd: fixture.cwd, Provider: "fixture:preflight", Config: task.TaskConfig{Permission: config.ModeReadOnly}}, Dependencies{Catalog: preflightFixtureCatalog(t, fixture.candidate), Environment: []string{}}.normalized(), doctorBundle(t), nil)
		if code != 1 || report.Provider.Status != "observation_incomplete" {
			t.Fatalf("incomplete cause hidden: %+v code=%d", report, code)
		}
	}
}

func TestDoctorPreservesActionableStaticProviderRejections(t *testing.T) {
	for _, test := range []struct {
		name             string
		fault            error
		placement        bool
		workspaceOverlap bool
		code             int
		reason, step     string
	}{
		{name: "authentication", fault: commonprovider.ErrAuthenticationBlocked, code: 2, reason: "authentication_unavailable", step: "native login"},
		{name: "placement", placement: true, code: 2, reason: "state_placement_rejected", step: "state root outside"},
		{name: "workspace overlap", workspaceOverlap: true, code: 2, reason: "state_placement_rejected", step: "state root outside"},
		{name: "authentication with incomplete read", fault: errors.Join(commonprovider.ErrAuthenticationBlocked, &os.PathError{Op: "open", Path: "private", Err: os.ErrPermission}), code: 1, reason: "authentication_unavailable", step: "native login"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			fixture := preflightFixture{cwd: t.TempDir(), prepareFault: test.fault}
			if test.workspaceOverlap {
				root = filepath.Join(fixture.cwd, "state")
			}
			catalog := preflightFixtureCatalog(t, func(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
				candidate, err := fixture.candidate(request, environment)
				if test.placement {
					candidate.WritableRoots = []string{root}
				}
				return candidate, err
			})
			report, code := observeDoctor(Arguments{Root: root, Cwd: fixture.cwd, Provider: "fixture:preflight", Config: task.TaskConfig{Permission: config.ModeReadOnly}}, Dependencies{Catalog: catalog, Environment: []string{}}.normalized(), doctorBundle(t), nil)
			if code != test.code || report.Provider.Authentication != "unknown" || len(report.Issues) != 1 || report.Issues[0].Code != test.reason || !strings.Contains(report.Issues[0].NextStep, test.step) {
				t.Fatalf("static facts/action lost: %+v code=%d", report, code)
			}
		})
	}
}
