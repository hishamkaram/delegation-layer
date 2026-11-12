package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// DoctorReport owns only ephemeral observations. Installation, persisted binding,
// and provider configuration remain authoritative in their existing owners.
// Consumers must not infer launch compatibility or authentication from this report.
type DoctorReport struct {
	SchemaVersion int                `json:"schema_version"`
	Command       string             `json:"command"`
	Root          DoctorRoot         `json:"root"`
	Installation  DoctorInstallation `json:"installation"`
	Supervisor    DoctorSupervisor   `json:"supervisor"`
	Scheduler     DoctorScheduler    `json:"scheduler"`
	Provider      *DoctorProvider    `json:"provider,omitempty"`
	Issues        []DoctorIssue      `json:"issues"`
}

// DoctorScheduler is advisory; its issues never change doctor exit policy.
type DoctorScheduler struct {
	Compatibility scheduler.Compatibility `json:"compatibility"`
	Verification  string                  `json:"verification"`
	Issues        []DoctorIssue           `json:"issues"`
}

type DoctorRoot struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}
type DoctorInstallation struct {
	Scope         string                `json:"scope"`
	Verification  string                `json:"verification"`
	Compatibility string                `json:"compatibility"`
	Status        string                `json:"status"`
	Roles         map[string]DoctorRole `json:"roles"`
}
type DoctorRole struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}
type DoctorSupervisor struct {
	Binding    string `json:"binding"`
	Liveness   string `json:"liveness"`
	ActiveWork string `json:"active_work"`
}
type DoctorProvider struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Executable     string `json:"executable"`
	Authentication string `json:"authentication"`
	Verification   string `json:"verification"`
}
type DoctorIssue struct {
	Code     string `json:"code"`
	Area     string `json:"area"`
	NextStep string `json:"next_step"`
}

func runDoctor(a Arguments, deps Dependencies, stdout, stderr io.Writer) int {
	self, err := os.Executable()
	report, code := observeDoctor(a, deps, self, err)
	if a.JSON {
		data, encodeErr := json.Marshal(report)
		if encodeErr != nil {
			return writeProviderError(stderr, encodeErr)
		}
		if len(data)+1 > MaxJSONResponseBytes {
			return writeProviderError(stderr, ErrJSONResponseTooLarge)
		}
		if err = writeAll(stdout, append(data, '\n')); err != nil {
			return 1
		}
	} else {
		if err = writeDoctorHuman(stdout, report); err != nil {
			return 1
		}
	}
	return code
}

func observeDoctor(a Arguments, deps Dependencies, self string, selfErr error) (DoctorReport, int) {
	report := DoctorReport{
		SchemaVersion: 1, Command: "doctor", Issues: []DoctorIssue{},
		Installation: DoctorInstallation{Scope: "installation_bundle", Verification: "filesystem_metadata", Compatibility: "not_checked", Status: "observed", Roles: map[string]DoctorRole{}},
		Supervisor:   DoctorSupervisor{Binding: "observation_incomplete", Liveness: "not_checked", ActiveWork: "unknown"},
	}
	observation := doctorObservation{report: report}
	observation.installation(self, selfErr)
	observation.root(a.Root)
	observation.scheduler(deps)
	if a.Provider != "" {
		observation.provider(a, deps)
	}
	return observation.report, observation.code
}

type doctorObservation struct {
	report DoctorReport
	code   int
}

func (o *doctorObservation) issue(area, reason, next string, incomplete bool) {
	o.report.Issues = append(o.report.Issues, DoctorIssue{Code: reason, Area: area, NextStep: next})
	if incomplete {
		o.code = 1
	} else if o.code == 0 {
		o.code = 2
	}
}

func (o *doctorObservation) installation(self string, selfErr error) {
	report := &o.report
	if selfErr != nil {
		report.Installation.Status = "observation_incomplete"
		for _, name := range []string{"delegate", "delegate_run", "pueue", "pueued"} {
			report.Installation.Roles[name] = DoctorRole{Status: "observation_incomplete"}
		}
		o.issue("installation", "observation_incomplete", "Retry from the selected installed Delegate executable.", true)
	} else {
		roles := installation.ObserveBundle(self)
		for _, name := range []string{"delegate", "delegate_run", "pueue", "pueued"} {
			role := roles[name]
			status := installation.BundleRoleStatus(role.Err)
			report.Installation.Roles[name] = DoctorRole{Path: role.Path, Status: status}
			if status == "available" {
				continue
			}
			incomplete := status == "observation_incomplete"
			if incomplete {
				report.Installation.Status = "observation_incomplete"
			} else if report.Installation.Status != "observation_incomplete" {
				report.Installation.Status = "blocked"
			}
			o.issue(name, "installation_unavailable", "Inspect the selected installation permissions and restore missing or unusable roles using its original installation method.", incomplete)
		}
	}
}

func (o *doctorObservation) root(requested string) {
	report := &o.report
	root, err := tasklifecycle.ResolveRoot(requested)
	report.Root = DoctorRoot{Path: root, Status: "observation_incomplete"}
	if err != nil {
		o.issue("root", "observation_incomplete", "Check the absolute state root and its parent directory permissions.", true)
	} else if info, statErr := os.Stat(root); errors.Is(statErr, os.ErrNotExist) {
		report.Root.Status = "not_initialized"
		report.Supervisor.Binding = "not_initialized"
	} else if statErr != nil || !info.IsDir() {
		o.issue("root", "observation_incomplete", "Check that the selected state root is an accessible directory.", true)
	} else {
		report.Root.Status = "present"
		_, found, bindingErr := pueue.ReadPrivateSupervisorBinding(root)
		switch {
		case bindingErr != nil:
			o.issue("supervisor", "observation_incomplete", "Inspect the persisted private supervisor binding before using this state root.", true)
		case found:
			report.Supervisor.Binding = "present"
		default:
			report.Supervisor.Binding = "binding_absent"
		}
	}
}

func (o *doctorObservation) provider(a Arguments, deps Dependencies) {
	report := &o.report
	root := report.Root.Path
	report.Provider = &DoctorProvider{ID: a.Provider, Status: "blocked", Authentication: "unknown", Verification: "static_preflight"}
	if root == "" {
		report.Provider.Status = "observation_incomplete"
	} else {
		observed, providerErr := doctorProvider(a, deps, root)
		report.Provider.Executable = observed.ProviderExecutable
		if providerErr == nil {
			report.Provider.Status = "static_ready"
		} else {
			incomplete := doctorIncomplete(providerErr)
			if incomplete {
				report.Provider.Status = "observation_incomplete"
			}
			reason := staticPreflightRejection(observed, providerErr)
			o.issue("provider", reason, doctorProviderNextStep(reason), incomplete)
		}
	}
}

func doctorProvider(a Arguments, deps Dependencies, root string) (admissioncore.PreflightResult, error) {
	_, cwd, err := config.ValidateDirectories(root, a.Cwd)
	if err != nil {
		return admissioncore.PreflightResult{}, err
	}
	request, err := admissioncore.BuildRequest(strings.Repeat("0", 32), strings.Repeat("1", 32), a.Provider, cwd, a.Config, []byte("doctor"), nil)
	if err != nil {
		return admissioncore.PreflightResult{}, err
	}
	return admissioncore.Preflight(staticAdmissionOptions(deps), root, request)
}

func doctorIncomplete(err error) bool {
	return task.AnyErrorNode(err, func(node error) bool {
		var pathErr *os.PathError
		if errors.As(node, &pathErr) && !errors.Is(pathErr.Err, os.ErrNotExist) {
			return true
		}
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		return errors.As(node, &syntaxErr) || errors.As(node, &typeErr) || errors.Is(node, io.ErrUnexpectedEOF) || errors.Is(node, io.EOF)
	})
}

func writeDoctorHuman(w io.Writer, report DoctorReport) error {
	var output strings.Builder
	fmt.Fprintf(&output, "doctor: installation=%s compatibility=%s\nroot=%s status=%s\nsupervisor binding=%s liveness=%s active_work=%s\n", report.Installation.Status, report.Installation.Compatibility, report.Root.Path, report.Root.Status, report.Supervisor.Binding, report.Supervisor.Liveness, report.Supervisor.ActiveWork)
	for _, name := range []string{"delegate", "delegate_run", "pueue", "pueued"} {
		role := report.Installation.Roles[name]
		fmt.Fprintf(&output, "%s=%s path=%s\n", name, role.Status, role.Path)
	}
	if report.Provider != nil {
		fmt.Fprintf(&output, "provider=%s status=%s authentication=%s\n", report.Provider.ID, report.Provider.Status, report.Provider.Authentication)
	}
	fmt.Fprintf(&output, "scheduler compatibility=%s verification=%s\n", report.Scheduler.Compatibility, report.Scheduler.Verification)
	for _, issue := range report.Scheduler.Issues {
		fmt.Fprintf(&output, "%s: %s; %s\n", issue.Area, issue.Code, issue.NextStep)
	}
	for _, issue := range report.Issues {
		fmt.Fprintf(&output, "%s: %s; %s\n", issue.Area, issue.Code, issue.NextStep)
	}
	return writeAll(w, []byte(output.String()))
}

func doctorProviderNextStep(reason string) string {
	switch reason {
	case "authentication_unavailable":
		return "Sign in using the selected provider CLI native login flow, then rerun doctor; live authentication remains unknown."
	case "state_placement_rejected":
		return "Use a workspace outside the state root that does not contain it, or run doctor directly with --root selecting a state root outside the provider workspace and all provider writable directories."
	default:
		return "Check the selected provider installation, workspace and required provider options; live authentication remains unknown."
	}
}

func (o *doctorObservation) scheduler(deps Dependencies) {
	compatibility := scheduler.CompatibilityObservationIncomplete
	if o.report.Root.Path != "" && o.report.Root.Status != "observation_incomplete" {
		compatibility = scheduler.ObserveCompatibility(context.Background(), o.report.Root.Path, deps.storeDependencies().registry())
	}
	observed := DoctorScheduler{Compatibility: compatibility, Verification: "read_only", Issues: []DoctorIssue{}}
	var next string
	switch compatibility {
	case scheduler.CompatibilityAbsent, scheduler.CompatibilityCompatible:
	case scheduler.CompatibilityMigrationRequired:
		next = "Use a compatible Delegate scheduler owner to migrate this database before using scheduler commands."
	case scheduler.CompatibilityNewerSchema:
		next = "Use a Delegate build that supports this scheduler database; downgrading does not reverse migrations."
	case scheduler.CompatibilityIncompatible:
		next = "Inspect the scheduler database identity, schema and journal mode with its owning Delegate installation; doctor does not repair it."
	case scheduler.CompatibilityObservationIncomplete:
		next = "Retry when scheduler metadata is accessible and no recovery or exclusive writer blocks this read; doctor does not recover journals."
	}
	if next != "" {
		observed.Issues = append(observed.Issues, DoctorIssue{Area: "scheduler", Code: string(compatibility), NextStep: next})
	}
	o.report.Scheduler = observed
}
