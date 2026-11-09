package guardian

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOutputSilenceCauseIsBoundToStopMessage(t *testing.T) {
	message := lifetimeMessage{Version: lifetimeProtocolVersion, Type: lifetimeMessageStop, TaskID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Cause: task.StopCauseOutputSilence}
	if err := validateLifetimeMessage(message); err != nil {
		t.Fatal(err)
	}
	message.Type = lifetimeMessageClose
	if err := validateLifetimeMessage(message); err == nil {
		t.Fatal("output silence cause accepted on completion")
	}
	message.Type = lifetimeMessageStop
	message.Cause = "untrusted"
	if err := validateLifetimeMessage(message); err == nil {
		t.Fatal("unknown stop cause accepted")
	}
}
