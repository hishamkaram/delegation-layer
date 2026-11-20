package gatewaycommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type reconciliationClient struct {
	commandClient
	inactive bool
}

func (c *reconciliationClient) RuntimeInactive(context.Context, string) (bool, error) {
	return c.inactive, nil
}

type reconciliationGate struct {
	calls  int
	cancel context.CancelFunc
}

type reconciliationCleanupError struct{ cause error }

func (e *reconciliationCleanupError) Error() string               { return "reconciliation cleanup fixture fault" }
func (e *reconciliationCleanupError) Unwrap() error               { return e.cause }
func (*reconciliationCleanupError) RetainedStore() *taskdir.Store { return nil }

func (g *reconciliationGate) WithQuiescentRoot(ctx context.Context, _ taskdir.RootPin, apply func(context.Context) error) error {
	g.calls++
	if g.cancel != nil {
		g.cancel()
		return ctx.Err()
	}
	return apply(ctx)
}

func reconciliationFixture(t *testing.T) (string, *reconciliationClient, Dependencies) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	store, err := taskdir.InitializeConnectorStoreContext(t.Context(), root, predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	pin, pinErr := store.PinRootContext(t.Context())
	if err = errors.Join(pinErr, store.Close()); err != nil {
		t.Fatal(err)
	}
	root = pin.Root()
	origin := task.RootContextCoordinates{Device: pin.Device() + 1, Inode: pin.Inode()}
	token, err := task.ConnectorRootToken(pin.RootID(), origin)
	if err != nil {
		t.Fatal(err)
	}
	inputs := infrastructure.StartupInputs{
		Kind: infrastructure.StartupRootOwner, Root: root, RootID: pin.RootID(), RootDevice: origin.Device, RootInode: origin.Inode,
		BootID:    "linux:11111111-2222-3333-4444-555555555555",
		Artifacts: infrastructure.Artifacts{Runner: filepath.Join(filepath.Dir(root), "installed-runner")},
		Context:   infrastructure.LaunchContextReference{SHA256: strings.Repeat("d", 64)},
	}
	digest, err := inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	intent := infrastructure.StartupIntent{Protocol: infrastructure.RecordProtocolVersion, Sequence: 1, Epoch: strings.Repeat("e", 32), Inputs: inputs, InputsSHA256: digest, AcceptedAt: "2026-11-03T00:00:00Z", Deadline: "2026-11-03T00:00:30Z"}
	if err = intent.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "infrastructure"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "infrastructure", "intent-00000000000000000001.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "gateway")
	if err = os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(profile, "profile.json"), []byte("retained-profile-and-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &reconciliationClient{inactive: true, commandClient: commandClient{local: deviceclient.LocalContextInfo{DelegateRoot: root, DelegateRootContext: token}}}
	return root, client, commandDependencies(t, client)
}

func runReconciliation(t *testing.T, root string, a Arguments, deps Dependencies) (Response, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(boundedTestContext(t), a, root, true, &stdout, &stderr, deps)
	var result Response
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("exit=%d output=%s diagnostic=%s: %v", code, &stdout, &stderr, err)
	}
	return result, code
}

func preparedReconciliation(t *testing.T, root string, deps Dependencies) Response {
	t.Helper()
	prepared, code := runReconciliation(t, root, Arguments{Action: "repair-context"}, deps)
	if code != 0 || prepared.RootContext == nil || prepared.RootContext.Origin.Device == prepared.RootContext.Accepted.Device || prepared.NextAction != "confirm_root_context_reconciliation" {
		t.Fatalf("prepare: exit%d %+v", code, prepared)
	}
	return prepared
}

func TestGatewayRootContextPrepareIsPassive(t *testing.T) {
	root, _, deps := reconciliationFixture(t)
	path := filepath.Join(root, "root.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	preparedReconciliation(t, root, deps)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("prepare changed root: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, ".supervisor", "bootstrap.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepare provisioned supervisor: %v", err)
	}
}

func TestGatewayRootContextConfirmationRequiresInactiveAttestedProfile(t *testing.T) {
	root, client, deps := reconciliationFixture(t)
	prepared := preparedReconciliation(t, root, deps)
	a := Arguments{Action: "repair-context", ConfirmContext: prepared.RootContext.Challenge}
	gate := &reconciliationGate{}
	deps.RootContextGate = gate
	refused, code := runReconciliation(t, root, a, deps)
	if code == 0 || refused.Failure != "root_cutover_required" || gate.calls != 0 {
		t.Fatalf("confirmation bypassed attestation: exit%d %+v gate%d", code, refused, gate.calls)
	}
	a.OfflineConfirmed = true
	client.inactive = false
	refused, code = runReconciliation(t, root, a, deps)
	if code != 3 || refused.Failure != "root_context_runtime_active" || gate.calls != 0 {
		t.Fatalf("active connector bypassed: exit%d %+v", code, refused)
	}
}

func TestGatewayRootContextPrepareAndConfirmedRepair(t *testing.T) {
	root, _, deps := reconciliationFixture(t)
	prepared := preparedReconciliation(t, root, deps)
	a := Arguments{Action: "repair-context", ConfirmContext: prepared.RootContext.Challenge, OfflineConfirmed: true}
	gate := &reconciliationGate{}
	deps.RootContextGate = gate
	confirmed, code := runReconciliation(t, root, a, deps)
	if code != 0 || confirmed.RootContext == nil || gate.calls != 1 || confirmed.NextAction != "run_gateway_doctor" {
		t.Fatalf("confirm: exit%d %+v gate%d", code, confirmed, gate.calls)
	}
	call := invocation{root: root, profile: filepath.Join(root, "gateway"), dependencies: deps}
	if _, err := call.selectedContext(t.Context()); err != nil {
		t.Fatalf("saved token still rejected: %v", err)
	}
	profile, err := os.ReadFile(filepath.Join(root, "gateway", "profile.json"))
	if err != nil || string(profile) != "retained-profile-and-credential\n" {
		t.Fatalf("profile rewritten: %v", err)
	}
	repeated, code := runReconciliation(t, root, a, deps)
	if code != 0 || repeated.RootContext == nil || !repeated.RootContext.AlreadyReconciled {
		t.Fatalf("idempotent confirm: exit%d %+v", code, repeated)
	}
}

func TestGatewayRootContextBindingOwnsOriginAfterRepair(t *testing.T) {
	root, _, deps := reconciliationFixture(t)
	prepared := preparedReconciliation(t, root, deps)
	deps.RootContextGate = &reconciliationGate{}
	_, code := runReconciliation(t, root, Arguments{Action: "repair-context", ConfirmContext: prepared.RootContext.Challenge, OfflineConfirmed: true}, deps)
	if code != 0 {
		t.Fatalf("first repair exit%d", code)
	}
	if err := os.WriteFile(filepath.Join(root, "infrastructure", "intent-00000000000000000001.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repeated := preparedReconciliation(t, root, deps)
	if repeated.RootContext.Origin != prepared.RootContext.Origin || !repeated.RootContext.AlreadyReconciled {
		t.Fatalf("binding lost original preimage: %+v", repeated.RootContext)
	}
}

func TestGatewayRootContextDoesNotExposeWireGuard(t *testing.T) {
	root, client, deps := reconciliationFixture(t)
	prepared := preparedReconciliation(t, root, deps)
	data, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"token_sha256", client.local.DelegateRootContext, task.ComputeSHA256([]byte(client.local.DelegateRootContext))} {
		if strings.Contains(string(data), private) {
			t.Fatal("root repair response exposes a raw token or wire guard")
		}
	}
}

func TestGatewayRootContextRuntimeErrorsRetainWholeCause(t *testing.T) {
	for _, test := range []struct {
		cause         error
		code          int
		failure, next string
	}{
		{supervisor.ErrRuntimeQuiescenceBusy, 3, "root_context_native_work_pending", "wait_for_native_work"},
		{os.ErrPermission, 1, "root_context_evidence_unreadable", "stop_and_report"},
		{context.Canceled, 1, "gateway_operation_interrupted", "inspect_same_profile"},
		{context.DeadlineExceeded, 1, "gateway_operation_interrupted", "inspect_same_profile"},
		{errors.Join(context.Canceled, context.DeadlineExceeded), 1, "gateway_operation_interrupted", "inspect_same_profile"},
		{errors.Join(context.Canceled, os.ErrPermission), 1, "root_context_evidence_unreadable", "stop_and_report"},
		{&reconciliationCleanupError{cause: context.Canceled}, 1, "root_context_evidence_unreadable", "stop_and_report"},
		{errors.Join(supervisor.ErrRuntimeQuiescenceBusy, os.ErrPermission), 1, "root_context_evidence_unreadable", "stop_and_report"},
		{&reconciliationCleanupError{cause: supervisor.ErrRuntimeQuiescenceBusy}, 1, "root_context_evidence_unreadable", "stop_and_report"},
	} {
		classified := rootContextNativeError(test.cause)
		if !errors.Is(classified, test.cause) {
			t.Fatal("runtime classification lost its cause")
		}
		failure, next, code := publicError(classified)
		if code != test.code || failure != test.failure || next != test.next {
			t.Fatalf("runtime error: code%d failure%s next%s", code, failure, next)
		}
	}
}

func TestGatewayRootContextNotRequiredCannotHideCleanupFault(t *testing.T) {
	failure, next, code := publicError(taskdir.ErrRootContextNotRequired)
	if failure != "root_context_not_required" || next != "run_gateway_doctor" || code != 1 {
		t.Fatalf("no-op guidance: %s %s exit%d", failure, next, code)
	}
	failure, next, code = publicError(errors.Join(taskdir.ErrRootContextNotRequired, os.ErrPermission))
	if failure != "root_context_evidence_unreadable" || next != "stop_and_report" || code != 1 {
		t.Fatalf("no-op hid cleanup fault: %s %s exit%d", failure, next, code)
	}
}

func TestGatewayRootContextPrepareMalformedRequestIsEvidenceFault(t *testing.T) {
	root, _, deps := reconciliationFixture(t)
	dir := filepath.Join(root, "tasks", strings.Repeat("a", 32))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte("{\"partial\":"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	response, code := runReconciliation(t, root, Arguments{Action: "repair-context"}, deps)
	if code != 1 || response.Failure != "root_context_evidence_unreadable" || response.NextAction != "stop_and_report" {
		t.Fatalf("prepare hid malformed canonical evidence: exit%d %+v", code, response)
	}
	after, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed prepare changed root: %v", err)
	}
}

func TestGatewayRootContextStaleChallengeRequiresNewPrepare(t *testing.T) {
	root, _, deps := reconciliationFixture(t)
	prepared := preparedReconciliation(t, root, deps)
	challenge := "a" + prepared.RootContext.Challenge[1:]
	if challenge == prepared.RootContext.Challenge {
		challenge = "b" + challenge[1:]
	}
	deps.RootContextGate = &reconciliationGate{}
	before, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	response, code := runReconciliation(t, root, Arguments{Action: "repair-context", ConfirmContext: challenge, OfflineConfirmed: true}, deps)
	if code != 1 || response.Failure != "root_context_challenge_stale" || response.NextAction != "prepare_root_context_reconciliation" {
		t.Fatalf("stale challenge guidance: exit%d %+v", code, response)
	}
	after, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("stale confirmation changed root: %v", err)
	}
}

func TestGatewayRootContextMatchingLegacyDoesNotNeedHistory(t *testing.T) {
	root, client, deps := reconciliationFixture(t)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	observed, tokenErr := reader.InspectConnectorContext(t.Context(), "")
	if err = errors.Join(tokenErr, reader.Close()); err != nil {
		t.Fatal(err)
	}
	client.local.DelegateRootContext = observed.RootToken
	if err = os.Remove(filepath.Join(root, "infrastructure", "intent-00000000000000000001.json")); err != nil {
		t.Fatal(err)
	}
	response, code := runReconciliation(t, root, Arguments{Action: "repair-context"}, deps)
	if code != 1 || response.Failure != "root_context_not_required" {
		t.Fatalf("matching legacy context required history: exit%d %+v", code, response)
	}
}

func TestGatewayRootContextBindingMismatchKeepsIdentityGuidance(t *testing.T) {
	root, client, deps := reconciliationFixture(t)
	prepared := preparedReconciliation(t, root, deps)
	deps.RootContextGate = &reconciliationGate{}
	if response, code := runReconciliation(t, root, Arguments{Action: "repair-context", ConfirmContext: prepared.RootContext.Challenge, OfflineConfirmed: true}, deps); code != 0 {
		t.Fatalf("fixture repair: exit%d %+v", code, response)
	}
	wrong := strings.Repeat("a", 64)
	if wrong == client.local.DelegateRootContext {
		wrong = strings.Repeat("b", 64)
	}
	client.local.DelegateRootContext = wrong
	response, code := runReconciliation(t, root, Arguments{Action: "repair-context"}, deps)
	if code != 1 || response.Failure != "native_context_changed" || response.NextAction != "stop_and_report" {
		t.Fatalf("identity mismatch became unreadable evidence: exit%d %+v", code, response)
	}
}

func TestGatewayRootContextDefaultGateNeverInitializesSupervisor(t *testing.T) {
	root, _, deps := reconciliationFixture(t)
	prepared, code := runReconciliation(t, root, Arguments{Action: "repair-context"}, deps)
	if code != 0 || prepared.RootContext == nil {
		t.Fatalf("prepare: exit%d %+v", code, prepared)
	}
	before, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, code = runReconciliation(t, root, Arguments{Action: "repair-context", ConfirmContext: prepared.RootContext.Challenge, OfflineConfirmed: true}, deps)
	if code == 0 {
		t.Fatal("missing existing supervisor accepted")
	}
	after, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refusal changed root: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, ".supervisor", "bootstrap.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmation provisioned supervisor: %v", err)
	}
}

func TestGatewayRootContextConfirmationArguments(t *testing.T) {
	for _, input := range [][]string{{"--confirm", strings.Repeat("a", 64)}, {"--offline-cutover-confirmed"}, {"--confirm", "malformed", "--offline-cutover-confirmed"}} {
		a := Arguments{Action: "repair-context"}
		fs := flag.NewFlagSet("repair-context", flag.ContinueOnError)
		RegisterFlags(fs, &a)
		if err := fs.Parse(input); err != nil {
			t.Fatal(err)
		}
		if err := a.Validate(nil); err == nil {
			t.Fatalf("incomplete confirmation accepted: %v", input)
		}
	}
}

func TestGatewayRootContextPendingDoesNotHideFault(t *testing.T) {
	for _, pending := range []error{ErrRootContextRuntime, ErrRootContextTransition} {
		_, _, code := publicError(pending)
		if code != 3 {
			t.Fatalf("clean pending lost: %v exit%d", pending, code)
		}
		failure, next, code := publicError(errors.Join(pending, os.ErrPermission))
		if code != 1 || failure != "root_context_evidence_unreadable" || next != "stop_and_report" {
			t.Fatalf("joined fault hidden: %v code%d failure=%s next=%s", pending, code, failure, next)
		}
	}
	for _, pending := range []error{pueue.ErrPrivateUpgradePending, pueue.ErrPrivateRuntimeRestartPending} {
		if !rootContextTransitionOnly(pending) || rootContextTransitionOnly(errors.Join(pending, os.ErrPermission)) {
			t.Fatalf("transition/fault classification lost: %v", pending)
		}
	}
}

func TestGatewayRootContextCancelledConfirmIsInterruption(t *testing.T) {
	root, _, deps := reconciliationFixture(t)
	prepared := preparedReconciliation(t, root, deps)
	path := filepath.Join(root, "root.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(boundedTestContext(t))
	defer cancel()
	deps.RootContextGate = &reconciliationGate{cancel: cancel}
	var stdout, stderr bytes.Buffer
	code := Run(ctx, Arguments{Action: "repair-context", ConfirmContext: prepared.RootContext.Challenge, OfflineConfirmed: true}, root, true, &stdout, &stderr, deps)
	var response Response
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if code != 1 || response.Failure != "gateway_operation_interrupted" || response.NextAction != "inspect_same_profile" {
		t.Fatalf("cancelled confirmation: exit%d %+v", code, response)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("cancelled confirmation changed root record: %v", err)
	}
}
