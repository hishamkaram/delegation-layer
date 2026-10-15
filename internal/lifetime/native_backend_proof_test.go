//go:build linux || darwin

package lifetime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	nativeProofReceiptEnv     = "DELEGATE_NATIVE_LIFETIME_RECEIPT"
	nativeProofRevisionEnv    = "DELEGATE_NATIVE_PROOF_REVISION"
	nativeProofPackageHashEnv = "DELEGATE_NATIVE_PROOF_PACKAGE_SHA256"
	nativeProofGateHashEnv    = "DELEGATE_NATIVE_PROOF_GATE_SHA256"
	nativeProofReceiptLimit   = 16 * 1024
	nativeProofEvidenceLimit  = 8
)

type nativeBackendObservation struct {
	primitive       string
	observed        string
	scopeIdentifier string
}

type nativePrimitiveReceipt struct {
	SchemaVersion       int                  `json:"schema_version"`
	HostOS              string               `json:"host_os"`
	HostKernel          string               `json:"host_kernel"`
	Architecture        string               `json:"architecture"`
	Backend             Kind                 `json:"backend"`
	Assurance           Assurance            `json:"assurance"`
	Test                string               `json:"test"`
	Primitive           string               `json:"primitive"`
	Observed            string               `json:"observed"`
	PlanSHA256          string               `json:"plan_sha256"`
	ScopeIdentitySHA256 string               `json:"scope_identity_sha256"`
	Source              nativeSourceIdentity `json:"source"`
	Binary              nativeBinaryIdentity `json:"binary"`
	Verdict             string               `json:"verdict"`
}

type nativeSourceIdentity struct {
	Revision      string `json:"revision"`
	PackageSHA256 string `json:"package_sha256"`
	GateSHA256    string `json:"gate_sha256"`
}

type nativeBinaryIdentity struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	GoVersion string `json:"go_version"`
}

type nativeAssuranceReceipt struct {
	SchemaVersion          int       `json:"schema_version"`
	PrimitiveReceiptSHA256 string    `json:"primitive_receipt_sha256"`
	Assurance              Assurance `json:"assurance"`
	Owner                  string    `json:"owner"`
	Lease                  string    `json:"lease"`
	Proof                  string    `json:"proof"`
	UnresolvedLimits       []string  `json:"unresolved_limits"`
}

type nativeFailureDetails struct {
	Code    string `json:"code"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

type nativeBackendProofReceipt struct {
	SchemaVersion      int                    `json:"schema_version"`
	ClaimClass         string                 `json:"claim_class"`
	PrimitiveReceipt   nativePrimitiveReceipt `json:"primitive_receipt"`
	AssuranceReceipt   nativeAssuranceReceipt `json:"assurance_receipt"`
	LastStage          string                 `json:"last_stage"`
	CleanupStatus      string                 `json:"cleanup_status"`
	RetainedEvidence   []string               `json:"retained_evidence"`
	UnresolvedEvidence []string               `json:"unresolved_evidence"`
	Failure            nativeFailureDetails   `json:"failure"`
	Verdict            string                 `json:"verdict"`
}

func TestNativeBackendProof(t *testing.T) {
	outputPath := os.Getenv(nativeProofReceiptEnv)
	if outputPath == "" {
		t.Skip("native proof is exercised by make acceptance-native-lifetime; that gate rejects a missing receipt")
	}
	receipt := newNativeBackendProofReceipt(t, outputPath)
	if !filepath.IsAbs(outputPath) {
		t.Fatalf("%s must be an absolute path", nativeProofReceiptEnv)
	}
	populateNativeProofIdentities(t, receipt)
	plan := prepareNativeProofPlan(t, receipt)
	if !nativeProofExpected(plan) {
		t.Fatalf("Prepare selected unexpected native backend %s/%s on %s", plan.Kind(), plan.Assurance(), runtime.GOOS)
	}
	receipt.setStage("backend-exercise-started")
	observation := exerciseSelectedNativeBackend(t, plan, receipt)
	receipt.PrimitiveReceipt.Primitive = observation.primitive
	receipt.PrimitiveReceipt.Observed = observation.observed
	receipt.PrimitiveReceipt.ScopeIdentitySHA256 = nativeProofDigest(observation.scopeIdentifier)
	receipt.PrimitiveReceipt.Verdict = "passed"
	receipt.setStage("backend-exercise-complete")
	receipt.AssuranceReceipt = nativeProofAssurance(t, receipt.PrimitiveReceipt, plan)
	receipt.setStage("scope-released")
}

func newNativeBackendProofReceipt(t *testing.T, outputPath string) *nativeBackendProofReceipt {
	t.Helper()
	receipt := &nativeBackendProofReceipt{
		SchemaVersion:      1,
		ClaimClass:         "native-lifetime",
		LastStage:          "receipt-finalizer-installed",
		CleanupStatus:      "pending",
		RetainedEvidence:   []string{},
		UnresolvedEvidence: []string{},
		Failure: nativeFailureDetails{
			Code:    "pending",
			Stage:   "receipt-finalizer-installed",
			Message: "native lifetime proof is still running",
		},
		Verdict: "inconclusive",
		PrimitiveReceipt: nativePrimitiveReceipt{
			SchemaVersion: 1,
			HostOS:        runtime.GOOS,
			Architecture:  runtime.GOARCH,
			Test:          "TestNativeBackendProof",
			Verdict:       "inconclusive",
		},
	}
	t.Cleanup(func() {
		finalizeNativeBackendProofReceipt(t, receipt)
		if err := writeNativeBackendProofReceipt(outputPath, *receipt); err != nil {
			t.Errorf("write native backend proof receipt: %v", err)
		}
	})
	return receipt
}

func (r *nativeBackendProofReceipt) setStage(stage string) {
	if r == nil || stage == "" {
		return
	}
	r.LastStage = stage
}

func (r *nativeBackendProofReceipt) addEvidence(target *[]string, value string) {
	if r == nil || target == nil || value == "" {
		return
	}
	for _, existing := range *target {
		if existing == value {
			return
		}
	}
	if len(*target) >= nativeProofEvidenceLimit {
		return
	}
	*target = append(*target, value)
}

func nativeProofScopeIdentity(scope *Scope) string {
	if scope == nil {
		return "unknown"
	}
	return nativeProofDigest(scope.Reference().Identifier())
}

func (r *nativeBackendProofReceipt) setFailure(code, stage, message string) {
	if r == nil {
		return
	}
	r.Failure = nativeFailureDetails{Code: code, Stage: stage, Message: message}
}

func recordNativeProofDestroyFailure(receipt *nativeBackendProofReceipt, scope *Scope) {
	if receipt == nil {
		return
	}
	receipt.setStage("cleanup-destroy")
	receipt.CleanupStatus = "scope-retained"
	receipt.addEvidence(&receipt.RetainedEvidence, "native-scope:"+nativeProofScopeIdentity(scope))
	receipt.addEvidence(&receipt.RetainedEvidence, "native-scope-handle:release-pending")
	receipt.addEvidence(&receipt.UnresolvedEvidence, "destroy-empty-failed")
	receipt.setFailure("destroy-empty-failed", receipt.LastStage, "native scope and handle retained after failed destruction")
}

func recordNativeProofReleaseFailure(receipt *nativeBackendProofReceipt, scope *Scope) {
	if receipt == nil {
		return
	}
	receipt.setStage("cleanup-release")
	receipt.CleanupStatus = "handle-retained"
	receipt.addEvidence(&receipt.RetainedEvidence, "native-scope-handle:release-pending")
	receipt.addEvidence(&receipt.UnresolvedEvidence, "scope-handle-release-failed")
	receipt.addEvidence(&receipt.UnresolvedEvidence, "native-scope:"+nativeProofScopeIdentity(scope))
	receipt.setFailure("scope-handle-release-failed", receipt.LastStage, "native scope handle retained after failed release")
}

func recordNativeProofReleased(receipt *nativeBackendProofReceipt) {
	if receipt == nil {
		return
	}
	receipt.CleanupStatus = "released"
	receipt.setStage("scope-released")
}

func finalizeNativeBackendProofReceipt(t *testing.T, receipt *nativeBackendProofReceipt) {
	t.Helper()
	if receipt.LastStage == "" {
		receipt.LastStage = "receipt-finalizer-installed"
	}
	if receipt.CleanupStatus == "pending" {
		receipt.CleanupStatus = "not-attempted"
		receipt.addEvidence(&receipt.UnresolvedEvidence, "native-scope-cleanup-not-reached")
	}
	failed := t.Failed()
	if !failed && receipt.CleanupStatus != "released" {
		t.Errorf("native proof finalization did not release the scope")
		receipt.setFailure("incomplete-finalization", receipt.LastStage, "native proof did not reach released cleanup status")
		failed = true
	}
	if failed {
		if receipt.Failure.Code == "pending" || receipt.Failure.Code == "none" {
			receipt.setFailure("test-failed", receipt.LastStage, "native lifetime proof test failed")
		}
		receipt.Verdict = "failed"
		receipt.PrimitiveReceipt.Verdict = "failed"
	} else {
		receipt.Failure = nativeFailureDetails{Code: "none", Stage: "", Message: ""}
		receipt.Verdict = "passed"
		receipt.PrimitiveReceipt.Verdict = "passed"
	}
	if receipt.AssuranceReceipt.SchemaVersion != 0 {
		primitiveBytes, err := json.Marshal(receipt.PrimitiveReceipt)
		if err != nil {
			t.Errorf("rebind primitive receipt: %v", err)
		} else {
			receipt.AssuranceReceipt.PrimitiveReceiptSHA256 = nativeProofBytesDigest(primitiveBytes)
		}
	}
}

func populateNativeProofIdentities(t *testing.T, receipt *nativeBackendProofReceipt) {
	t.Helper()
	kernel, err := nativeProofKernel(t.TempDir())
	if err != nil {
		t.Fatalf("read native kernel identity: %v", err)
	}
	receipt.PrimitiveReceipt.HostKernel = kernel
	revision := os.Getenv(nativeProofRevisionEnv)
	packageHash := os.Getenv(nativeProofPackageHashEnv)
	gateHash := os.Getenv(nativeProofGateHashEnv)
	if !isNativeProofRevision(revision) || !isNativeProofDigest(packageHash) || !isNativeProofDigest(gateHash) {
		t.Fatal("native proof source identity is missing or malformed")
	}
	receipt.PrimitiveReceipt.Source = nativeSourceIdentity{
		Revision: revision, PackageSHA256: packageHash, GateSHA256: gateHash,
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve native proof test binary: %v", err)
	}
	binaryHash, binarySize, err := hashNativeProofFile(executable, 256*1024*1024)
	if err != nil {
		t.Fatalf("fingerprint native proof test binary: %v", err)
	}
	receipt.PrimitiveReceipt.Binary = nativeBinaryIdentity{
		Name: filepath.Base(executable), SHA256: binaryHash, SizeBytes: binarySize, GoVersion: runtime.Version(),
	}
	receipt.setStage("identities-populated")
}

func prepareNativeProofPlan(t *testing.T, receipt *nativeBackendProofReceipt) Plan {
	t.Helper()
	plan, err := Prepare("native-proof", "test", "backend-"+strconv.Itoa(os.Getpid()))
	if err != nil {
		t.Fatalf("Prepare selected native backend: %v", err)
	}
	receipt.PrimitiveReceipt.Backend = plan.Kind()
	receipt.PrimitiveReceipt.Assurance = plan.Assurance()
	receipt.PrimitiveReceipt.PlanSHA256 = nativeProofDigest(string(plan.Kind()) + "\x00" + string(plan.Assurance()) + "\x00" + plan.Identifier())
	receipt.setStage("plan-prepared")
	return plan
}

func nativeProofAssurance(t *testing.T, primitive nativePrimitiveReceipt, plan Plan) nativeAssuranceReceipt {
	t.Helper()
	primitiveBytes, err := json.Marshal(primitive)
	if err != nil {
		t.Fatalf("encode primitive receipt for assurance binding: %v", err)
	}
	return nativeAssuranceReceipt{
		SchemaVersion:          1,
		PrimitiveReceiptSHA256: nativeProofBytesDigest(primitiveBytes),
		Assurance:              plan.Assurance(),
		Owner:                  "native proof test process",
		Lease:                  "test invocation owns the created scope until cleanup",
		Proof:                  "production Prepare selected the backend; CreatePrepared created it; the observed child was waited and the scope was proven empty before destruction",
		UnresolvedLimits:       nativeProofLimits(plan.Kind()),
	}
}

func nativeProofKernel(workDir string) (string, error) {
	command := exec.Command("uname", "-sr")
	command.Dir = workDir
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	kernel := strings.TrimSpace(string(output))
	if len(kernel) == 0 || len(kernel) > 128 {
		return "", fmt.Errorf("kernel identity has invalid length")
	}
	for _, char := range kernel {
		if !strings.ContainsRune(" .-_0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", char) {
			return "", fmt.Errorf("kernel identity contains an unsupported character")
		}
	}
	return kernel, nil
}

func hashNativeProofFile(path string, maximum int64) (digest string, size int64, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		return "", 0, fmt.Errorf("file size or type is outside the native proof bound")
	}
	hasher := sha256.New()
	readBytes, err := io.Copy(hasher, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", 0, err
	}
	if readBytes != info.Size() {
		return "", 0, fmt.Errorf("file size changed during native proof fingerprinting")
	}
	return hex.EncodeToString(hasher.Sum(nil)), info.Size(), nil
}

func writeNativeBackendProofReceipt(path string, receipt nativeBackendProofReceipt) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("receipt path is not absolute")
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > nativeProofReceiptLimit {
		return fmt.Errorf("receipt exceeds %d-byte limit", nativeProofReceiptLimit)
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	if err = os.Rename(temporary, path); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	return nil
}

func nativeProofDigest(value string) string {
	return nativeProofBytesDigest([]byte(value))
}

func nativeProofBytesDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func isNativeProofDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isNativeProofRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func nativeProofLimits(kind Kind) []string {
	limits := []string{
		"fixture_only_no_live_provider_acceptance",
		"application_lifetime_owner_lease_not_exercised",
	}
	if kind == KindProcessGroup {
		limits = append(limits, "cooperative_scope_owner_loss_remains_unverifiable")
	} else {
		limits = append(limits, "cgroup_proof_limited_to_recorded_kernel_and_exercised_primitive")
	}
	return limits
}

func exerciseNativeProcessGroup(t *testing.T, plan Plan, primitive string, receipt *nativeBackendProofReceipt) nativeBackendObservation {
	t.Helper()
	if plan.Kind() != KindProcessGroup || plan.Assurance() != AssuranceCooperative {
		t.Fatalf("process-group probe received plan %s/%s", plan.Kind(), plan.Assurance())
	}
	scope, err := CreatePrepared(plan)
	if err != nil {
		t.Fatalf("CreatePrepared process-group plan: %v", err)
	}
	fixtureDir := t.TempDir()
	command := exec.Command("sleep", "5")
	command.Dir = fixtureDir
	fixture := &nativeProcessGroupFixture{scope: scope, cmd: command, receipt: receipt}
	t.Cleanup(func() { cleanupNativeProcessGroupFixture(t, fixture) })
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatalf("BindOwnerProcess: %v", err)
	}
	if err = scope.Configure(fixture.cmd); err != nil {
		t.Fatalf("Configure selected process-group scope: %v", err)
	}
	if err = fixture.cmd.Start(); err != nil {
		t.Fatalf("start selected process-group child: %v", err)
	}
	launchRef, err := scope.ObserveStarted(fixture.cmd.Process.Pid)
	if err != nil {
		t.Fatalf("observe selected process-group child: %v", err)
	}
	fixture.launchRef = launchRef
	fixture.launchObserved = true
	terminateNativeProcessGroupFixture(t, fixture)
	if err := scope.DestroyEmpty(); err != nil {
		recordNativeProofDestroyFailure(receipt, scope)
		t.Fatalf("DestroyEmpty selected process-group scope: %v", err)
	}
	fixture.destroyed = true
	if err := scope.Release(); err != nil {
		recordNativeProofReleaseFailure(receipt, scope)
		t.Fatalf("Release selected process-group scope: %v", err)
	}
	fixture.released = true
	recordNativeProofReleased(receipt)
	scopeIdentifier := scope.Reference().Identifier()
	return nativeBackendObservation{
		primitive:       primitive,
		observed:        "prepared child terminated; command Wait consumed once; scope empty and destroyed",
		scopeIdentifier: scopeIdentifier,
	}
}

type nativeProcessGroupFixture struct {
	scope          *Scope
	cmd            *exec.Cmd
	launchRef      LaunchRef
	launchObserved bool
	waited         bool
	destroyed      bool
	released       bool
	receipt        *nativeBackendProofReceipt
}

func terminateNativeProcessGroupFixture(t *testing.T, fixture *nativeProcessGroupFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	terminateErr := fixture.scope.Terminate(ctx, []LaunchRef{fixture.launchRef})
	cancel()
	if terminateErr != nil {
		t.Fatalf("Terminate selected process-group child: %v", terminateErr)
	}
	waitErr := fixture.cmd.Wait()
	fixture.waited = true
	if waitErr == nil {
		t.Fatal("terminated process-group child returned nil Wait error")
	}
	empty, err := fixture.scope.Empty([]LaunchRef{fixture.launchRef})
	if err != nil {
		t.Fatalf("Empty selected process-group scope: %v", err)
	}
	if !empty {
		t.Fatal("selected process-group scope remained populated after Wait")
	}
}

func cleanupNativeProcessGroupFixture(t *testing.T, fixture *nativeProcessGroupFixture) {
	t.Helper()
	if fixture.cmd.Process != nil && !fixture.waited {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		refs := []LaunchRef(nil)
		if fixture.launchObserved {
			refs = []LaunchRef{fixture.launchRef}
		}
		if err := fixture.scope.Terminate(ctx, refs); err != nil {
			t.Errorf("cleanup process-group Terminate: %v", err)
		}
		cancel()
		if err := fixture.cmd.Wait(); err != nil {
			t.Logf("cleanup process-group child exited with: %v", err)
		}
		fixture.waited = true
	}
	if !fixture.destroyed {
		if err := fixture.scope.DestroyEmpty(); err != nil {
			if fixture.receipt != nil {
				recordNativeProofDestroyFailure(fixture.receipt, fixture.scope)
			}
			t.Errorf("cleanup process-group DestroyEmpty: %v", err)
			return
		}
		fixture.destroyed = true
	}
	if !fixture.released {
		if err := fixture.scope.Release(); err != nil {
			if fixture.receipt != nil {
				recordNativeProofReleaseFailure(fixture.receipt, fixture.scope)
			}
			t.Errorf("cleanup process-group Release: %v", err)
			return
		}
		fixture.released = true
		recordNativeProofReleased(fixture.receipt)
	}
}

func nativeProofExpected(plan Plan) bool {
	switch runtime.GOOS {
	case "linux":
		return plan.Kind() == KindCgroupV2 && plan.Assurance() == AssuranceExact ||
			plan.Kind() == KindProcessGroup && plan.Assurance() == AssuranceCooperative
	case "darwin":
		return plan.Kind() == KindProcessGroup && plan.Assurance() == AssuranceCooperative
	default:
		return false
	}
}
