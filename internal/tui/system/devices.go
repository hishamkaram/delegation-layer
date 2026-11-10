package system

import (
	"context"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (v view) deviceActions() []ui.Action {
	var actions []ui.Action
	for _, operation := range []string{"list", "probe", "trust"} {
		a := v.defaults("devices", operation)
		fields := []ui.Field{ui.TextField("SSH config", "optional explicit SSH config file", "", func(a *app.Arguments, s string) { a.SSHConfig = s })}
		if operation != "list" {
			fields = append(fields, deviceTargetFields()...)
		}
		if operation == "trust" {
			fields = append(fields, ui.TextField("Host fingerprint", "exact expected host key fingerprint, independently verified", "", func(a *app.Arguments, s string) { a.Fingerprint = s }))
		}
		kind := session.Read
		if operation != "list" {
			kind = session.Effect
		}
		actions = append(actions, ui.Action{ID: "devices-" + operation, Label: "Devices: " + operation, Description: "SSH configuration remains user-owned. Trust requires the exact independently verified fingerprint.", Arguments: a, Fields: fields, Kind: kind, Confirm: operation == "trust", Reconciliation: deviceReadback, Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
			result := v.backend.Device(ctx, a)
			feedback := ui.Result(result)
			if result.Remote == nil {
				return feedback
			}
			feedback.Title = "Devices: " + a.RemoteAction
			feedback.Body = fmt.Sprintf("Connectivity: %s\nDevices: %s\n%s", result.Remote.Connectivity, strings.Join(result.Remote.Devices, ", "), feedback.Body)
			for _, stage := range result.Remote.Stages {
				feedback.Body += fmt.Sprintf("\n%+v", stage)
			}
			return feedback
		}})
	}
	return actions
}

func deviceTargetFields() []ui.Field {
	return []ui.Field{
		ui.TextField("SSH alias", "existing SSH configuration alias", "", func(a *app.Arguments, s string) { a.Device = s }),
		ui.TextField("Remote root", "optional absolute destination state root", "", func(a *app.Arguments, s string) { a.RemoteRoot = s }),
		ui.TextField("Remote directory", "optional prepared destination workspace", "", func(a *app.Arguments, s string) { a.RemoteCwd = s }),
		ui.TextField("Remote executable", "optional installed Delegate path", "", func(a *app.Arguments, s string) { a.RemoteExecutable = s }),
		ui.TextField("Remote provider", "optional provider for explicit probe", "", func(a *app.Arguments, s string) { a.Provider = s }),
	}
}

func deviceReadback(a app.Arguments) []string {
	if a.Device == "" {
		return readback(a, "devices", "list")
	}
	result := readback(a, "devices", "probe", "--device", a.Device)
	for _, option := range []struct{ name, value string }{{"--ssh-config", a.SSHConfig}, {"--remote-root", a.RemoteRoot}, {"--remote-cwd", a.RemoteCwd}, {"--remote-executable", a.RemoteExecutable}} {
		if option.value != "" {
			result = append(result, option.name, option.value)
		}
	}
	return result
}
