package system

import (
	"context"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (v view) discoveryActions() []ui.Action {
	a := v.defaults("doctor")
	return []ui.Action{{ID: "doctor", Label: "Installation doctor", Description: "Read installation and static readiness; this does not launch a provider.", Arguments: a, Kind: session.Effect, Run: func(_ context.Context, a app.Arguments) ui.Feedback { return v.doctor(a) }}}
}

func (v view) providerActions(id string) []ui.Action {
	models := v.defaults("models")
	models.Provider = id
	models.Cwd = v.cwd
	preflight := v.defaults("preflight")
	preflight.Provider = id
	preflight.Cwd = v.cwd
	doctor := v.defaults("doctor")
	doctor.Provider = id
	doctor.Cwd = v.cwd
	return []ui.Action{
		{ID: "provider-doctor", Label: "Provider doctor", Description: "Inspect installation and static readiness for this exact provider/context without a provider turn.", Arguments: doctor, Kind: session.Effect, Fields: doctorFields(v.cwd), Run: func(_ context.Context, a app.Arguments) ui.Feedback { return v.doctor(a) }},
		{ID: "capabilities", Label: "Declared capabilities", Arguments: app.Arguments{Provider: id}, Kind: session.Read, Run: func(_ context.Context, a app.Arguments) ui.Feedback {
			response, err := v.backend.Capabilities(a.Provider)
			c := response.Capability
			return ui.Feedback{Title: "Capabilities", Body: fmt.Sprintf("Provider: %s\nVerification: %s\nModes: %s\nOptions: %s\nContinuation: %s\nReason: %s", c.Provider, c.Verification, strings.Join(c.SupportedModes, ", "), strings.Join(c.SupportedOptions, ", "), c.Continuation, c.ReasonCode), Err: err}
		}},
		{ID: "models", Label: "Discover models", Description: "Explicit advisory discovery may check/launch the selected provider; it does not create a task.", Arguments: models, Kind: session.Effect, Confirm: true, Fields: []ui.Field{ui.TextField("Working directory", "absolute discovery context; blank uses the current directory", v.cwd, func(a *app.Arguments, s string) { a.Cwd = s })}, Reconciliation: func(a app.Arguments) []string { return readback(a, "doctor", "--provider", a.Provider, "--cwd", a.Cwd) }, Run: func(_ context.Context, a app.Arguments) ui.Feedback {
			response, result := v.backend.Models(a)
			feedback := ui.Result(result)
			var text strings.Builder
			fmt.Fprintf(&text, "Provider: %s\nStatus: %s\nObserved: %s\nComplete: %t\nReason: %s\n", response.Provider, response.Status, response.ObservedAt, response.Complete, response.ReasonCode)
			for _, model := range response.Models {
				fmt.Fprintf(&text, "\n%s %s\n  Effort: %s", model.ID, model.Name, strings.Join(model.Efforts, ", "))
			}
			feedback.Title = "Models"
			if response.Failure != nil {
				fmt.Fprintf(&text, "\nFailure: %s\nNext: %s\n", response.Failure.Code, response.Failure.NextAction)
			}
			feedback.Body = text.String() + "\n" + feedback.Body
			return feedback
		}},
		{ID: "preflight", Label: "Explicit preflight", Description: "This can launch a provider check, but creates no task. Preserve the exact requested provider/model/permissions.", Arguments: preflight, Kind: session.Effect, Confirm: true, Fields: preflightFields(v.cwd), Run: func(ctx context.Context, a app.Arguments) ui.Feedback { return ui.Result(v.backend.Preflight(ctx, a)) }},
	}
}

func preflightFields(cwd string) []ui.Field {
	return []ui.Field{
		ui.TextField("Working directory", "absolute provider workspace", cwd, func(a *app.Arguments, s string) { a.Cwd = s }),
		ui.TextField("Permission", "native permission intent", "read-only", func(a *app.Arguments, s string) { a.Config.Permission = s }),
		ui.TextField("Model", "exact model; blank keeps provider default", "", func(a *app.Arguments, s string) { a.Config.Model = s }),
		ui.TextField("Effort", "exact effort; blank keeps default", "", func(a *app.Arguments, s string) { a.Config.Effort = s }),
		ui.TextField("Budget", "duration; blank uses existing default", "", func(a *app.Arguments, s string) { a.Config.Budget = s }),
		ui.TextField("Native timeout", "optional provider-native timeout", "", func(a *app.Arguments, s string) { a.Config.NativeTimeout = s }),
		ui.TextField("Review target", "optional uncommitted, base:BRANCH or commit:SHA", "", func(a *app.Arguments, s string) { a.ReviewTarget = s }),
	}
}

func (v view) doctor(a app.Arguments) ui.Feedback {
	report, code := v.backend.Doctor(a)
	var text strings.Builder
	fmt.Fprintf(&text, "Root: %s (%s)\nInstallation: %s\nCompatibility: %s\nSupervisor: %s; liveness %s; work %s", report.Root.Path, report.Root.Status, report.Installation.Status, report.Installation.Compatibility, report.Supervisor.Binding, report.Supervisor.Liveness, report.Supervisor.ActiveWork)
	for _, issue := range report.Issues {
		fmt.Fprintf(&text, "\n%s (%s): %s", issue.Code, issue.Area, issue.NextStep)
	}
	feedback := ui.Feedback{Title: "Doctor", Body: text.String()}
	if code != 0 {
		feedback.Err = fmt.Errorf("doctor reported issues (exit %d)", code)
	}
	return feedback
}

func doctorFields(cwd string) []ui.Field {
	return []ui.Field{
		ui.TextField("Working directory", "absolute context for this provider", cwd, func(a *app.Arguments, s string) { a.Cwd = s }),
		ui.TextField("Model", "optional requested model", "", func(a *app.Arguments, s string) { a.Config.Model = s }),
		ui.TextField("Effort", "optional requested effort", "", func(a *app.Arguments, s string) { a.Config.Effort = s }),
		ui.TextField("Review target", "optional uncommitted, base:BRANCH or commit:SHA", "", func(a *app.Arguments, s string) { a.ReviewTarget = s }),
	}
}
