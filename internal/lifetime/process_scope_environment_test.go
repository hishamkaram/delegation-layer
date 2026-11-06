package lifetime

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
)

func TestWithoutProcessScopeMarkerOwnsExactKeyRemoval(t *testing.T) {
	input := []string{"PROVIDER=" + string([]byte{0xff}), processScopeMarkerEnvironment + "=A", processScopeMarkerEnvironment + "=duplicate", processScopeMarkerEnvironment, processScopeMarkerEnvironment + "_NEAR=keep", "OTHER=" + processScopeMarkerEnvironment}
	before := slices.Clone(input)
	got := WithoutProcessScopeMarker(input)
	expected := []string{before[0], before[4], before[5]}
	if !slices.Equal(got, expected) {
		t.Fatal("scope removal changed other keys or retained reserved key")
	}
	got[0] = "changed"
	if !slices.Equal(input, before) {
		t.Fatal("scope removal aliases caller")
	}
	if WithoutProcessScopeMarker(nil) != nil {
		t.Fatal("nil changed to explicit empty")
	}
	empty := WithoutProcessScopeMarker([]string{})
	if empty == nil || len(empty) != 0 {
		t.Fatal("explicit empty changed to inherited nil")
	}
}

func TestConfigureProcessScopeEnvironmentReplacesReservedMarker(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"PROVIDER=keep", processScopeMarkerEnvironment + "=old", processScopeMarkerEnvironment + "=duplicate"}}
	if err := configureProcessScopeEnvironment(cmd, "new"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Env, []string{"PROVIDER=keep", processScopeMarkerEnvironment + "=new"}) {
		t.Fatal("configuration retained stale markers")
	}
	empty := &exec.Cmd{Env: []string{}}
	if err := configureProcessScopeEnvironment(empty, "new"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(empty.Env, []string{processScopeMarkerEnvironment + "=new"}) {
		t.Fatal("explicit empty inherited ambient environment")
	}
}

func TestConfigureProcessScopeEnvironmentPreservesInheritanceAndRefusal(t *testing.T) {
	if err := configureProcessScopeEnvironment(nil, "new"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("nil command: %v", err)
	}
	cmd := &exec.Cmd{}
	expected := append(WithoutProcessScopeMarker(os.Environ()), processScopeMarkerEnvironment+"=new")
	if err := configureProcessScopeEnvironment(cmd, "new"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Env, expected) {
		t.Fatal("nil environment stopped inheriting selected ambient values")
	}
	before := slices.Clone(cmd.Env)
	if err := configureProcessScopeEnvironment(cmd, "bad=value"); !errors.Is(err, ErrInvalidRef) || !slices.Equal(cmd.Env, before) {
		t.Fatalf("invalid marker changed environment: %v", err)
	}
}
