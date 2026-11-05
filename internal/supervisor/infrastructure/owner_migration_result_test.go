package infrastructure

import (
	"bufio"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestMigrationResultIsClosed(t *testing.T) {
	for _, result := range []MigrationResult{"", "unknown", MigrationCompleted, MigrationWarmReuse} {
		valid := result == MigrationCompleted || result == MigrationWarmReuse
		if err := result.Validate(); (err == nil) != valid {
			t.Fatalf("result %q: %v", result, err)
		}
	}
}

func TestMigrationResultIsIndependentOfDisposition(t *testing.T) {
	for _, test := range []struct {
		code    ownerReplyCode
		allowed []MigrationResult
	}{
		{ownerOK, []MigrationResult{MigrationCompleted, MigrationWarmReuse}},
		{ownerFault, []MigrationResult{"", MigrationCompleted, MigrationWarmReuse}},
		{ownerTimeout, []MigrationResult{"", MigrationCompleted, MigrationWarmReuse}},
		{ownerPending, []MigrationResult{""}},
		{ownerBusy, []MigrationResult{""}},
		{ownerRefused, []MigrationResult{""}},
		{ownerIncompatible, []MigrationResult{""}},
	} {
		for _, result := range []MigrationResult{"", "unknown", MigrationCompleted, MigrationWarmReuse} {
			request := ownerPreparationSubjectFixture(ownerMigrateReferenced)
			reply := ownerServerReply(request, test.code)
			reply.Target = originRecord{Root: t.TempDir(), RootID: request.RootID}
			reply.Startup = request.Owner
			reply.Disposition = Disposition{Kind: DispositionWarmReuse}
			reply.MigrationResult = result
			if err := reply.validate(); (err == nil) != slices.Contains(test.allowed, result) {
				t.Fatalf("code=%s result=%q: %v", test.code, result, err)
			}
		}
	}
}

func TestUnrelatedOwnerActionsRejectMigrationResults(t *testing.T) {
	for _, action := range []ownerAction{ownerStatus, ownerEnsureOwn, ownerEnsureOther, ownerRecoverSaved} {
		reply := ownerServerReply(ownerPreparationSubjectFixture(action), ownerFault)
		reply.MigrationResult = MigrationCompleted
		if err := reply.validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("unrelated %s accepted result: %v", action, err)
		}
	}
}

func TestOwnerWireVersionMismatchPrecedesVersionSpecificStrictFields(t *testing.T) {
	old := strings.Replace(ownerWireReplyJSON, `"protocol":2`, `"protocol":1`, 1)
	old = strings.Replace(old, `"code":"ok"`, `"code":"ok","old_version_only":true`, 1)
	if _, err := readOwnerFrame[ownerReply](bufio.NewReader(strings.NewReader(old))); !errors.Is(err, ErrOwnerIncompatible) {
		t.Fatalf("old protocol reported generic decode failure: %v", err)
	}
	current := strings.Replace(old, `"protocol":1`, `"protocol":2`, 1)
	if _, err := readOwnerFrame[ownerReply](bufio.NewReader(strings.NewReader(current))); !errors.Is(err, ErrIdentity) || errors.Is(err, ErrOwnerIncompatible) {
		t.Fatalf("current version lost strict unknown-field rejection: %v", err)
	}
	if ProtocolVersion != 1 {
		t.Fatal("owner wire change modified durable record schema")
	}
}
