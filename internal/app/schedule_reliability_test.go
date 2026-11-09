package app

import (
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestSchedulePolicyAndHealthReadsArePassive(t *testing.T) {
	root, _, _ := scheduleFixture(t)
	initial, code, _ := runScheduleTest(t, root, "policy", "get", scheduleTestID)
	if code != 0 || !reflect.DeepEqual(initial.SchedulePolicy, &contract.QuotaPolicy{MaxHold: 6 * time.Hour}) {
		t.Fatalf("unsafe default policy %+v code=%d", initial, code)
	}
	changed, code, _ := runScheduleTest(t, root, "policy", "set", scheduleTestID, "--quota-hold", "on", "--quota-max-hold", "2m")
	if code != 0 || !reflect.DeepEqual(changed.SchedulePolicy, &contract.QuotaPolicy{Enabled: true, MaxHold: 2 * time.Minute, Revision: 1}) {
		t.Fatalf("policy not persisted %+v code=%d", changed, code)
	}
	before := legacyScheduleSnapshot(t, root)
	health, code, _ := runScheduleTest(t, root, "health", scheduleTestID)
	if code != 0 || !reflect.DeepEqual(health.ScheduleHealth, &contract.Health{ScheduleID: scheduleTestID, Complete: true}) {
		t.Fatalf("invalid empty health %+v code=%d", health, code)
	}
	_, code, _ = runScheduleTest(t, root, "policy", "get", scheduleTestID)
	if code != 0 || !reflect.DeepEqual(before, legacyScheduleSnapshot(t, root)) {
		t.Fatal("passive health/policy query mutated state")
	}
}

func TestScheduleCreationParsesImmutableHookAndSilenceOptions(t *testing.T) {
	args := []string{"schedule", "create", "--brief", "brief.md", "--cwd", t.TempDir(), "--cron", "* * * * *", "--provider", "claude:print", "--failure-hook", "/installed/notify", "--failure-threshold", "3", "--failure-hook-timeout", "4s", "--output-silence-timeout", "250ms"}
	a, err := ParseArguments(args)
	if err != nil || a.ScheduleHook == nil || a.ScheduleHook.Executable != "/installed/notify" || a.ScheduleHook.Threshold != 3 || a.ScheduleHook.Timeout != 4*time.Second || a.Config.OutputSilenceTimeout != "250ms" {
		t.Fatalf("creation options lost %+v %v", a, err)
	}
}
