package guardian

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func pathProjectLaunchDirectory(t *testing.T, constrained, alias bool) (*ProjectDirectory, string) {
	t.Helper()
	project := projectLaunchDirectory(t)
	ancestor := filepath.Dir(project.Context.Observed.Path)
	project.Context.Binding = task.ConnectorContext{
		RootToken: strings.Repeat("a", 64),
		Project:   task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: project.Context.Observed.Path},
	}
	if constrained {
		token, err := taskdir.ProjectToken(task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: ancestor})
		if err != nil {
			t.Fatal(err)
		}
		project.Context.Binding.WorkspaceConstraint = &task.WorkspaceConstraint{Token: token, RelativeProject: "project"}
		if alias {
			if err = os.Symlink("project", filepath.Join(ancestor, "inside")); err != nil {
				t.Fatal(err)
			}
			project.Context.Binding.WorkspaceConstraint.RelativeProject = "inside"
		}
	}
	return project, ancestor
}

func replacePathLaunchDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestPathProjectLaunchGateSelectsCurrentDirectoryAfterAuthorization(t *testing.T) {
	for _, scenario := range []string{"leaf", "constrained_leaf", "ancestor", "inside_alias", "inside_retarget", "outside_retarget", "ancestor_alias", "leaf_alias", "mixed_binding"} {
		t.Run(scenario, func(t *testing.T) { assertPathProjectLaunchSelection(t, scenario) })
	}
}

func assertPathProjectLaunchSelection(t *testing.T, scenario string) {
	t.Helper()
	constrained := scenario != "leaf" && scenario != "leaf_alias"
	alias := scenario == "inside_alias" || scenario == "inside_retarget" || scenario == "outside_retarget"
	project, ancestor := pathProjectLaunchDirectory(t, constrained, alias)
	marker := filepath.Join(t.TempDir(), "provider.json")
	if scenario == "mixed_binding" {
		physical, err := taskdir.ObserveProjectContext(t.Context(), ancestor)
		if err != nil {
			t.Fatal(err)
		}
		token, err := taskdir.ProjectToken(physical)
		if err != nil {
			t.Fatal(err)
		}
		project.Context.Binding.WorkspaceConstraint.Token = token
	}
	process := startProjectLaunchProcess(t, project, marker)
	if len(process.command.ExtraFiles) != 1 {
		t.Fatal("path launch inherited project custody")
	}
	refused := mutatePathProjectSelection(t, project, ancestor, scenario)
	var expected task.DirectoryIdentity
	if !refused {
		var err error
		expected, err = taskdir.ObserveProjectContext(t.Context(), project.Context.Binding.Project.Path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := process.gate.Release(); err != nil {
		t.Fatal(err)
	}
	process.waited = true
	assertProjectLaunchProbe(t, expected, marker, refused, process.command.Wait())
	if _, err := project.File.Stat(); err != nil {
		t.Fatal("path launch closed borrowed observation:", err)
	}
}

func mutatePathProjectSelection(t *testing.T, project *ProjectDirectory, ancestor, scenario string) bool {
	t.Helper()
	refused := false
	switch scenario {
	case "leaf", "constrained_leaf":
		replacePathLaunchDirectory(t, project.Context.Binding.Project.Path)
	case "ancestor":
		replacePathLaunchDirectory(t, ancestor)
		if err := os.Mkdir(filepath.Join(ancestor, "project"), 0o700); err != nil {
			t.Fatal(err)
		}
	case "inside_alias":
		// An alias still resolving to the saved canonical leaf is valid.
	case "inside_retarget", "outside_retarget":
		refused = true
		target := t.TempDir()
		if scenario == "inside_retarget" {
			target = filepath.Join(ancestor, "other")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		link := filepath.Join(ancestor, "inside")
		if err := errors.Join(os.Remove(link), os.Symlink(target, link)); err != nil {
			t.Fatal(err)
		}
	case "ancestor_alias", "leaf_alias":
		refused = true
		path := ancestor
		if scenario == "leaf_alias" {
			path = project.Context.Binding.Project.Path
		}
		if err := os.Rename(path, path+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+"-moved", path); err != nil {
			t.Fatal(err)
		}
	case "mixed_binding":
		refused = true
	default:
		t.Fatal("unknown path launch scenario")
	}
	return refused
}

func TestProjectLaunchGateLegacyArgumentsRemainExact(t *testing.T) {
	project := projectLaunchDirectory(t)
	identity := project.Context.Observed
	encodedPath, err := json.Marshal(identity.Path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := project.arguments(7)
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON := fmt.Sprintf("{\"path\":%s,\"device\":%d,\"inode\":%d}\n", encodedPath, identity.Device, identity.Inode)
	want := []string{"--project-fd", "7", "--project-identity", legacyJSON}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy project argv = %q, want %q", got, want)
	}
}

func TestPathProjectLaunchGateStrictWireForms(t *testing.T) {
	project, _ := pathProjectLaunchDirectory(t, true, false)
	encoded, err := task.MarshalCanonical(project.Context.Binding)
	if err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(t.TempDir(), "provider")
	valid := []string{lifetimeLaunchGateFD, "3", lifetimeProjectPath, string(encoded), "--", provider, "provider"}
	objectPrefix := strings.TrimSuffix(strings.TrimSpace(string(encoded)), "}")
	parsed, err := parseLifetimeLaunchGateArguments(valid)
	if err != nil || !reflect.DeepEqual(parsed.pathContext, &project.Context.Binding) || parsed.projectFD != 0 {
		t.Fatalf("path context changed across wire: parsed=%+v err=%v", parsed, err)
	}
	legacy, err := projectLaunchDirectory(t).arguments(4)
	if err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"unknown_mode":     {lifetimeLaunchGateFD, "3", "--project-path-context-v99", string(encoded), "--", provider, "provider"},
		"unknown_field":    {lifetimeLaunchGateFD, "3", lifetimeProjectPath, objectPrefix + `,"unknown":true}`, "--", provider, "provider"},
		"duplicate_field":  {lifetimeLaunchGateFD, "3", lifetimeProjectPath, objectPrefix + `,"root_token":"` + strings.Repeat("a", 64) + `"}`, "--", provider, "provider"},
		"trailing_json":    {lifetimeLaunchGateFD, "3", lifetimeProjectPath, string(encoded) + " {}", "--", provider, "provider"},
		"oversize":         {lifetimeLaunchGateFD, "3", lifetimeProjectPath, strings.Repeat(" ", task.MaxControlRecordSize+1), "--", provider, "provider"},
		"path_then_fd":     slices.Concat(valid[:4], legacy, valid[4:]),
		"fd_then_path":     slices.Concat(valid[:2], legacy, valid[2:]),
		"missing_provider": valid[:5],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLifetimeLaunchGateArguments(args); err == nil {
				t.Fatal("accepted invalid path wire form")
			}
		})
	}
}

func TestPathProjectLaunchGateRefusesPhysicalIdentityFlag(t *testing.T) {
	project, _ := pathProjectLaunchDirectory(t, false, false)
	encoded, err := task.MarshalCanonical(project.Context.Binding.Project)
	if err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(t.TempDir(), "provider")
	if _, err = parseLifetimeLaunchGateArguments([]string{lifetimeLaunchGateFD, "3", lifetimeProjectFD, "4", lifetimeProjectID, string(encoded), "--", provider, "provider"}); err == nil {
		t.Fatal("path binding accepted in historical physical flag")
	}
}

// This fixture preserves the old decoder's flag grammar. An old runner must
// refuse the new mode before it can consume authorization or exec a provider.
func oldProjectLaunchDecoderAccepts(args []string) bool {
	if len(args) < 5 || args[0] != "--gate-fd" {
		return false
	}
	fd, err := strconv.Atoi(args[1])
	if err != nil || fd < 3 {
		return false
	}
	remaining := args[2:]
	if remaining[0] == "--project-fd" {
		if len(remaining) < 7 || remaining[2] != "--project-identity" {
			return false
		}
		remaining = remaining[4:]
	}
	return len(remaining) >= 3 && remaining[0] == "--"
}

func TestPathProjectLaunchOldRunnerRefusesNewMode(t *testing.T) {
	project, _ := pathProjectLaunchDirectory(t, false, false)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "provider.json")
	if !oldProjectLaunchDecoderAccepts([]string{"--gate-fd", "3", "--", executable, executable}) {
		t.Fatal("old decoder fixture rejects its supported grammar")
	}
	command := exec.Command(executable, "-test.run=^TestProjectLaunchProvider$")
	command.Env = append(os.Environ(), "DELEGATE_PROJECT_LAUNCH_MARKER="+marker, "DELEGATE_PROJECT_LAUNCH_OLD_DECODER=1")
	gate, err := NewLaunchGateFactory(executable)(command, project)
	if err != nil {
		t.Fatal(err)
	}
	process := &projectLaunchProcess{command: command, gate: gate}
	t.Cleanup(func() { process.close(t) })
	if oldProjectLaunchDecoderAccepts(command.Args[2:]) {
		t.Fatal("old decoder accepted PATH argv")
	}
	if err = command.Start(); err != nil {
		t.Fatal(errors.Join(err, gate.Started(err)))
	}
	process.started = true
	if err = gate.Started(nil); err != nil {
		t.Fatal(err)
	}
	// No positive token is needed: refusal precedes the gate read.
	process.waited = true
	assertProjectLaunchProbe(t, task.DirectoryIdentity{}, marker, true, command.Wait())
}
