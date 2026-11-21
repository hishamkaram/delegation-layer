package gatewaycommand

import (
	"flag"
	"io"
	"testing"
)

func TestGatewayConnectAndWorkspaceRejectExecutionPolicyFlags(t *testing.T) {
	for _, action := range []Arguments{{Action: "connect"}, {Action: "workspace", WorkspaceAction: "allow"}, {Action: "workspace", WorkspaceAction: "update"}} {
		for _, option := range []struct{ name, value string }{{"provider", "claude:print"}, {"model", "explicit-model"}, {"effort", "high"}, {"permission", "read-only"}, {"max-budget", "1m"}} {
			t.Run(action.Action+"/"+action.WorkspaceAction+"/"+option.name, func(t *testing.T) {
				arguments := action
				flags := flag.NewFlagSet("gateway", flag.ContinueOnError)
				flags.SetOutput(io.Discard)
				RegisterFlags(flags, &arguments)
				if err := flags.Parse([]string{"--" + option.name, option.value}); err == nil {
					t.Fatalf("removed execution policy flag %q is still accepted", option.name)
				}
			})
		}
	}
}
