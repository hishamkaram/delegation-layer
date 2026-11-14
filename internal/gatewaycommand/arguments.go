// Package gatewaycommand owns Native's Gateway command application. The SDK
// owns its profile/network, taskdir owns Native state and gatewayservice owns
// per-user service definitions. This package coordinates those public owners.
package gatewaycommand

import (
	"errors"
	"flag"
	"net/url"
	"path/filepath"
	"time"
)

const defaultMaximumBudget = 60 * time.Second

// Arguments are requested choices; parsing grants no local or remote authority.
type Arguments struct {
	Action           string
	WorkspaceAction  string
	ServiceAction    string
	GatewayURL       string
	Profile          string
	DeviceName       string
	PrivateCA        string
	Headless         bool
	Foreground       bool
	Revoke           bool
	TokenFile        string
	Replace          bool
	Directory        string
	Alias            string
	WorkspaceID      string
	ExpectedRevision uint64
	Provider         string
	Model            string
	Effort           string
	Permission       string
	MaximumBudget    time.Duration
	policySet        bool
	permissionSet    bool
	budgetSet        bool
	modelSet         bool
	effortSet        bool
}

func RegisterFlags(fs *flag.FlagSet, a *Arguments) {
	fs.StringVar(&a.Profile, "profile", "", "explicit private Gateway profile directory (default: selected Native root/gateway)")
	switch a.Action {
	case "connect":
		fs.StringVar(&a.DeviceName, "name", "", "device display name (default: local hostname)")
		fs.StringVar(&a.PrivateCA, "ca", "", "explicit private HTTPS CA file")
		fs.BoolVar(&a.Headless, "headless", false, "print the same Device Flow challenge without opening a browser")
		fs.BoolVar(&a.Foreground, "foreground", false, "serve in this terminal after enrollment instead of installing a user service")
		fs.StringVar(&a.TokenFile, "token-file", "", "unattended enrollment token JSON file, or - for stdin")
		fs.BoolVar(&a.Replace, "replace", false, "explicit credential replacement for this same selected profile")
		registerPolicyFlags(fs, a)
	case "disconnect":
		fs.BoolVar(&a.Revoke, "revoke", false, "after confirmed local connector stop, request fresh login to revoke this device credential")
		fs.BoolVar(&a.Headless, "headless", false, "print the revocation login challenge without opening a browser; requires --revoke")
	case "workspace":
		switch a.WorkspaceAction {
		case "allow":
			fs.StringVar(&a.Alias, "alias", "", "explicit workspace alias (default: valid directory basename)")
			registerPolicyFlags(fs, a)
		case "update":
			fs.StringVar(&a.Alias, "alias", "", "new alias for the same immutable workspace")
			fs.Uint64Var(&a.ExpectedRevision, "expected-revision", 0, "current workspace revision")
			registerPolicyFlags(fs, a)
		case "remove":
			fs.Uint64Var(&a.ExpectedRevision, "expected-revision", 0, "current workspace revision")
		}
	}
}

func registerPolicyFlags(fs *flag.FlagSet, a *Arguments) {
	permission, budget := "", time.Duration(0)
	providerHelp := "select one provider; omission keeps the approved provider set"
	if a.Action == "connect" {
		permission, budget = "read-only", defaultMaximumBudget
		providerHelp = "explicit eligible provider profile (default: compiled eligible profiles)"
	}
	fs.StringVar(&a.Provider, "provider", "", providerHelp)
	fs.StringVar(&a.Model, "model", "", "allow only this explicit model; requires --provider")
	fs.StringVar(&a.Effort, "effort", "", "allow only this explicit effort; requires --provider")
	fs.StringVar(&a.Permission, "permission", permission, "provider permission ceiling; workspace omission preserves approved policy")
	fs.DurationVar(&a.MaximumBudget, "max-budget", budget, "finite maximum task budget in whole seconds; workspace omission preserves approved policy")
}

// NoteFlags preserves nil policy meaning for workspace updates and approvals.
func (a *Arguments) NoteFlags(fs *flag.FlagSet) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "provider":
			a.policySet = true
		case "model":
			a.policySet, a.modelSet = true, true
		case "effort":
			a.policySet, a.effortSet = true, true
		case "permission":
			a.policySet, a.permissionSet = true, true
		case "max-budget":
			a.policySet, a.budgetSet = true, true
		}
	})
}

func (a *Arguments) Validate(positional []string) error {
	if a.Profile != "" && !filepath.IsAbs(a.Profile) {
		return errors.New("gateway profile must be absolute")
	}
	if (a.Model != "" || a.Effort != "" || a.modelSet || a.effortSet) && a.Provider == "" {
		return errors.New("gateway model/effort policy requires one explicit provider")
	}
	switch a.Action {
	case "connect":
		return a.validateConnect(positional)
	case "status", "doctor", "disconnect", "serve", "service":
		return a.validateSavedProfile(positional)
	case "workspace":
		return a.validateWorkspace(positional)
	default:
		return errors.New("unknown gateway action")
	}
}

func (a *Arguments) validateConnect(positional []string) error {
	if len(positional) != 1 {
		return errors.New("gateway connect requires one HTTPS Gateway URL")
	}
	a.GatewayURL = positional[0]
	return validGatewayURL(a.GatewayURL)
}

func (a *Arguments) validateSavedProfile(positional []string) error {
	if len(positional) != 0 {
		return errors.New("gateway observation/disconnect does not accept positional arguments")
	}
	if a.Action == "disconnect" && a.Headless && !a.Revoke {
		return errors.New("gateway disconnect --headless requires --revoke")
	}
	if a.Action == "service" {
		return a.validateServiceAction()
	}
	return nil
}

func (a *Arguments) validateServiceAction() error {
	switch a.ServiceAction {
	case "install", "start", "stop", "status", "uninstall":
		return nil
	default:
		return errors.New("unknown gateway service action")
	}
}

func (a *Arguments) validateWorkspace(positional []string) error {
	switch a.WorkspaceAction {
	case "list":
		if len(positional) != 0 {
			return errors.New("gateway workspace list does not accept positional arguments")
		}
	case "allow":
		if len(positional) != 1 {
			return errors.New("gateway workspace allow requires one existing local directory")
		}
		a.Directory = positional[0]
	case "update", "remove":
		if len(positional) != 1 || a.ExpectedRevision == 0 {
			return errors.New("gateway workspace update/remove requires an ID and expected-revision")
		}
		a.WorkspaceID = positional[0]
		if a.WorkspaceAction == "update" && a.Alias == "" && !a.policySet {
			return errors.New("gateway workspace update requires alias or explicit policy change")
		}
	default:
		return errors.New("unknown gateway workspace action")
	}
	return nil
}

func validGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("gateway URL must be a plain HTTPS origin")
	}
	return nil
}
