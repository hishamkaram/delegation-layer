package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func guardedContinuationCLIArguments(t *testing.T, f *connectorFixture, prior, fresh task.GatewayOrigin) []string {
	t.Helper()
	priorFile, freshFile := filepath.Join(t.TempDir(), "prior.json"), filepath.Join(t.TempDir(), "fresh.json")
	for path, origin := range map[string]task.GatewayOrigin{priorFile: prior, freshFile: fresh} {
		data, err := task.MarshalCanonical(origin)
		if err != nil {
			t.Fatal(err)
		}
		writeAppTestFile(t, path, data)
	}
	_, _, spec, _, err := f.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(f.projectToken))
	return []string{
		"continue", "--budget", "1m", "--task", f.request.TaskID, "--id", strings.Repeat("f", 32), "--root", f.store.Root,
		"--expected-root", f.rootToken, "--expected-project-sha256", hex.EncodeToString(hash[:]),
		"--expected-spec-sha256", spec, "--expected-origin", priorFile, "--origin", freshFile, "--json",
	}
}

func TestGuardedContinuationPublicParserKeepsDistinctIdentityInputs(t *testing.T) {
	f := newConnectorFixture(t)
	fresh := f.origin
	fresh.OperationID = "successor"
	args := guardedContinuationCLIArguments(t, f, f.origin, fresh)
	parsed, err := ParseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TaskID != f.request.TaskID || parsed.SuccessorTaskID != strings.Repeat("f", 32) || parsed.Connector.OriginFile == parsed.Connector.SuccessorOriginFile || parsed.Connector.ExpectedProjectSHA256 == "" {
		t.Fatalf("guarded identities changed during parse: %+v", parsed)
	}
	for _, flag := range []string{"--id", "--origin", "--expected-spec-sha256"} {
		without := omitContinuationFlag(args, flag)
		if _, err = ParseArguments(without); err == nil {
			t.Fatalf("guarded continue accepted missing %s", flag)
		}
	}
	bothStdin := slicesForContinuationFlags(args, map[string]string{"--origin": "-", "--expected-origin": "-"})
	if _, err = ParseArguments(bothStdin); err == nil {
		t.Fatal("two origin owners borrowed the same stdin")
	}
	ordinary, err := ParseArguments([]string{"continue", "--task", f.request.TaskID, "--budget", "1m"})
	if err != nil || hasConnectorGuards(ordinary.Connector) || ordinary.SuccessorTaskID != "" {
		t.Fatalf("normal continue acquired new requirements: %+v %v", ordinary, err)
	}
}

func omitContinuationFlag(args []string, flag string) []string {
	result := make([]string, 0, len(args)-2)
	for index := 0; index < len(args); index++ {
		if args[index] == flag {
			index++
			continue
		}
		result = append(result, args[index])
	}
	return result
}

func slicesForContinuationFlags(args []string, replacements map[string]string) []string {
	result := append([]string(nil), args...)
	for index := 0; index+1 < len(result); index++ {
		if value, ok := replacements[result[index]]; ok {
			result[index+1] = value
		}
	}
	return result
}

func TestGuardedContinuationPublicOriginsRejectBeforeTaskMutation(t *testing.T) {
	f := newConnectorFixture(t)
	fresh := f.origin
	fresh.OperationID = "successor"
	wrongPrior := f.origin
	wrongPrior.OperationID = "other-predecessor"
	cases := []struct {
		name         string
		prior, fresh task.GatewayOrigin
	}{
		{"wrong predecessor origin", wrongPrior, fresh},
		{"reused predecessor origin", f.origin, f.origin},
	}
	before := snapshotRebootTaskFiles(t, f.store.Root)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			args := guardedContinuationCLIArguments(t, f, test.prior, test.fresh)
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr, f.deps)
			var response Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("exit=%d stdout=%q stderr=%q: %v", code, &stdout, &stderr, err)
			}
			if code == 0 || response.TaskRecord == "created" {
				t.Fatalf("origin mismatch admitted successor: code=%d %+v", code, response)
			}
			if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, f.store.Root)) {
				t.Fatal("origin mismatch mutated durable task state")
			}
		})
	}
}

func TestSuccessorReceiptMatchesCanonicalSavedProjectHash(t *testing.T) {
	f := newConnectorFixture(t)
	identity, err := f.store.RecordedTaskIdentityContext(t.Context(), f.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := identity.ConnectorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(f.projectToken))
	a := Arguments{TaskID: f.request.TaskID, Root: f.store.Root, Connector: ConnectorArguments{ExpectedRoot: f.rootToken, ExpectedProjectSHA256: hex.EncodeToString(hash[:]), origin: f.origin.Clone()}}
	result := commandResult{response: newResponse("continue")}
	result.response.TaskRecord = "created"
	result = attachConnectorDispatchReceipt(t.Context(), a, f.deps, result)
	if result.code != 0 || result.err != nil || result.response.Connector == nil || !reflect.DeepEqual(result.response.Connector.Receipt, receipt) {
		t.Fatalf("canonical successor receipt did not match project hash: %+v", result)
	}
	// The hash comes from the exact token in durable evidence; there is no
	// current project lookup while matching the successor receipt.
	if err = os.Remove(f.request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	if err = matchConnectorReceipt(a, *receipt); err != nil {
		t.Fatal(err)
	}
}
