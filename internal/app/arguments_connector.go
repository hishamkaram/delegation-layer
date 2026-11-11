package app

import (
	"errors"
	"flag"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ConnectorArguments are requested guard/control values, never authority.
const defaultConnectorControlTimeout = 30 * time.Second

type ConnectorArguments struct {
	ControlTimeout           time.Duration
	Action                   string
	ExpectedRoot             string
	ExpectedProject          string
	OriginFile               string
	ExpectedSpecSHA256       string
	DescriptorFile           string
	ResumeAdmission          bool
	FenceIfAbsent            bool
	FenceUnattributedPartial bool
	OfflineCutoverConfirmed  bool
	GatewayID                string
	RecoveryID               string
	ExpectedGeneration       string
	Generation               string
	CompletenessReceipt      string
	Cursor                   string
	Limit                    int
	Offset                   int64
	ResultLimit              int64
	origin                   *task.GatewayOrigin
	binding                  *task.ConnectorContext
}

func registerConnectorFlags(fs *flag.FlagSet, a *Arguments) {
	c := &a.Connector
	fs.DurationVar(&c.ControlTimeout, "control-timeout", defaultConnectorControlTimeout, "finite public connector control deadline")
	fs.StringVar(&c.ExpectedRoot, "expected-root", "", "expected opaque root context token")
	fs.StringVar(&c.ExpectedProject, "expected-project", "", "expected opaque project context token")
	if a.Command == "dispatch" {
		fs.StringVar(&c.OriginFile, "origin", "", "strict immutable origin JSON file or - for stdin")
		return
	}
	fs.StringVar(&c.OriginFile, "expected-origin", "", "expected immutable origin JSON file or - for stdin")
	switch a.Command {
	case "resolve", "status", "collect", "cancel", "result":
		fs.StringVar(&c.ExpectedSpecSHA256, "expected-spec-sha256", "", "expected immutable Delegate spec digest")
	}
	switch a.Command {
	case "cancel":
		fs.StringVar(&a.remoteCancelID, "id", "", "stable cancellation control ID")
	case "context":
		fs.StringVar(&c.GatewayID, "gateway-id", "", "observe this explicitly registered gateway origin")
		fs.StringVar(&a.Cwd, "cwd", "", "existing canonical project directory")
		fs.BoolVar(&c.OfflineCutoverConfirmed, "offline-cutover-confirmed", false, "attest old writers/frontends are stopped and tasks drained")
	case "resolve":
		fs.BoolVar(&c.ResumeAdmission, "resume-admission", false, "authorize recovery of this existing complete prepared request only")
		fs.BoolVar(&c.FenceIfAbsent, "fence-if-absent", false, "permanently refuse absent task ID")
		fs.BoolVar(&c.FenceUnattributedPartial, "fence-unattributed-partial", false, "explicit LOCAL operator proof for selected unattributed partial ID")
	case "result":
		fs.StringVar(&c.DescriptorFile, "expected-descriptor", "", "expected committed payload descriptor JSON file or - for stdin")
		fs.Int64Var(&c.Offset, "offset", 0, "zero-based byte offset")
		fs.Int64Var(&c.ResultLimit, "limit", 65536, "maximum decoded result range bytes (1..65536)")
	case "origin":
		fs.StringVar(&c.GatewayID, "gateway-id", "", "stable gateway identity")
		fs.StringVar(&c.RecoveryID, "recovery-id", "", "explicit recovery transition identity")
		fs.StringVar(&c.ExpectedGeneration, "expected-generation", "", "prior generation; empty for first registration")
		fs.StringVar(&c.Generation, "generation", "", "fresh admission generation")
		fs.StringVar(&c.CompletenessReceipt, "completeness-receipt", "", "canonical inventory completeness proof")
		fs.StringVar(&c.Cursor, "cursor", "", "prior inventory task cursor")
		fs.IntVar(&c.Limit, "limit", 100, "inventory page limit (1..200)")
	}
}

func registerConnectorCommandFlags(fs *flag.FlagSet, a *Arguments) bool {
	switch a.Command {
	case "dispatch", "cancel", "status", "collect":
		registerConnectorFlags(fs, a)
		return false
	case "context", "resolve", "result", "origin":
		registerConnectorFlags(fs, a)
		return true
	default:
		return false
	}
}

func hasConnectorGuards(c ConnectorArguments) bool {
	return c.ExpectedRoot != "" || c.ExpectedProject != "" || c.OriginFile != "" || c.ExpectedSpecSHA256 != ""
}

func usesConnectorControl(a Arguments) bool {
	switch a.Command {
	case "context", "resolve", "result", "origin":
		return true
	default:
		return a.Connector.ExpectedRoot != ""
	}
}

func validateGuardedDispatchArguments(a *Arguments) error {
	if a.Connector.ExpectedSpecSHA256 != "" {
		return errors.New("dispatch does not accept expected spec; use guarded existing resolve")
	}
	if !hasConnectorGuards(a.Connector) {
		return nil
	}
	if err := validateConnectorArguments(a, nil); err != nil {
		return err
	}
	if a.Device != "" || a.Auto || a.ResumeTask != "" || a.WorkspaceID != "" || a.Isolation != "" || requestedTeamTarget(*a) != nil {
		return errors.New("connector dispatch requires frozen explicit provider/project and no alternate routing")
	}
	if a.TaskID == "" {
		return errors.New("connector dispatch requires explicit task ID")
	}
	return nil
}

func validateConnectorArguments(a *Arguments, positional []string) error {
	if a.Connector.OriginFile == "-" && a.Connector.DescriptorFile == "-" {
		return errors.New("origin and descriptor cannot both consume stdin")
	}
	switch a.Command {
	case "context":
		return validateConnectorContextArguments(*a, positional)
	case "origin":
		return validateConnectorOriginArguments(*a, positional)
	case "resolve", "result":
		if len(positional) != 1 {
			return errors.New("exactly one local task ID is required")
		}
		a.TaskID = positional[0]
		if err := task.ValidateTaskID(a.TaskID); err != nil {
			return err
		}
	}
	if err := validateConnectorResolveAction(a.Connector); err != nil {
		return err
	}
	if a.Connector.FenceUnattributedPartial {
		if a.Connector.ExpectedSpecSHA256 != "" {
			return task.ValidateSHA256(a.Connector.ExpectedSpecSHA256)
		}
		return nil
	}
	if err := validateConnectorGuardFields(*a); err != nil {
		return err
	}
	if a.Command == "result" {
		return validateConnectorResultArguments(a.Connector)
	}
	return nil
}

func validateConnectorContextArguments(a Arguments, positional []string) error {
	if a.Connector.ExpectedRoot != "" || a.Connector.ExpectedProject != "" || a.Connector.OriginFile != "" {
		return errors.New("context does not accept task context or origin guards")
	}
	if a.Connector.Action != "inspect" && a.Connector.Action != "enable-connector" {
		return errors.New("context requires inspect or enable-connector")
	}
	if a.Cwd != "" && !filepath.IsAbs(a.Cwd) {
		return errors.New("context cwd must be absolute")
	}
	if a.Connector.GatewayID != "" {
		if err := task.ValidateConnectorID(a.Connector.GatewayID); err != nil {
			return err
		}
	}
	return validateNoPositional("context", positional)
}

func validateConnectorOriginArguments(a Arguments, positional []string) error {
	c := a.Connector
	if c.ExpectedProject != "" || c.OriginFile != "" {
		return errors.New("origin does not accept task project or origin guards")
	}
	if c.Action != "barrier" && c.Action != "inventory" && c.Action != "reopen" {
		return errors.New("origin requires barrier, inventory or reopen")
	}
	if err := task.ValidateConnectorID(c.GatewayID); err != nil {
		return err
	}
	if err := task.ValidateConnectorID(c.Generation); err != nil {
		return err
	}
	if c.ExpectedRoot == "" {
		return errors.New("origin requires expected root context")
	}
	return validateNoPositional("origin", positional)
}

func validateConnectorResolveAction(c ConnectorArguments) error {
	actions := 0
	for _, selected := range []bool{c.ResumeAdmission, c.FenceIfAbsent, c.FenceUnattributedPartial} {
		if selected {
			actions++
		}
	}
	if actions > 1 {
		return errors.New("choose one explicit resolve action")
	}
	if c.ResumeAdmission && c.ExpectedSpecSHA256 == "" {
		return errors.New("existing recovery requires expected spec digest")
	}
	if c.FenceUnattributedPartial && (c.ExpectedRoot == "" || c.ExpectedProject == "" || c.OriginFile != "") {
		return errors.New("unattributed partial proof requires root/project guards and no guessed origin")
	}
	return nil
}

func validateConnectorResultArguments(c ConnectorArguments) error {
	if c.Action != "read" || c.DescriptorFile == "" || c.Offset < 0 || c.ResultLimit < 1 || c.ResultLimit > 65536 {
		return errors.New("result read requires expected descriptor, nonnegative offset and limit 1..65536")
	}
	return nil
}

func validateConnectorGuardFields(a Arguments) error {
	required := a.Command == "resolve" || a.Command == "result" || a.Command == "dispatch"
	if !required && !hasConnectorGuards(a.Connector) {
		return nil
	}
	if a.Connector.ExpectedRoot == "" || a.Connector.ExpectedProject == "" || a.Connector.OriginFile == "" {
		return errors.New("guarded task controls require expected root, project and immutable origin together")
	}
	if a.Connector.ExpectedSpecSHA256 != "" {
		return task.ValidateSHA256(a.Connector.ExpectedSpecSHA256)
	}
	return nil
}
