package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestContinuationAdmissionMissingEnvironmentOwnerRefusesBeforePreparation(t *testing.T) {
	fixture := preflightFixture{root: filepath.Join(t.TempDir(), "state"), cwd: t.TempDir()}
	options := staticAdmissionOptions(Dependencies{Catalog: preflightFixtureCatalog(t, fixture.candidate)})
	predecessor, err := admissioncore.BuildRequest(strings.Repeat("a", 32), strings.Repeat("b", 32), "fixture:preflight", fixture.cwd, task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"}, []byte("brief"), nil)
	if err != nil {
		t.Fatal(err)
	}
	successor := predecessor
	successor.TaskID = strings.Repeat("c", 32)
	prior := &task.PriorSession{Provider: predecessor.Provider, ConversationID: "recorded-session", PredecessorTaskID: predecessor.TaskID}
	_, err = admissioncore.ValidateContinuation(options, nil, fixture.root, predecessor, task.MetaRecord{EnvironmentRecorded: true}, prior, successor)
	if !errors.Is(err, task.ErrEvidenceFault) || fixture.prepareCalls != 0 || fixture.finalizeCalls != 0 {
		t.Fatalf("missing environment owner authorized static continuation: %v, calls=%d/%d", err, fixture.prepareCalls, fixture.finalizeCalls)
	}
	if _, err = os.Lstat(fixture.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing environment owner created state authority: %v", err)
	}
}
