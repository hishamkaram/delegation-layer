package antigravity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestInventorySourcesRetainsBaselineInputs(t *testing.T) {
	home, workspace := inventoryFixture(t)
	projectData, ruleData := populateInventoryBaseline(t, home, workspace)

	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	assertInventoryRoots(t, inventory, home, workspace)
	settings := requireInventorySource(t, inventory, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"))
	if settings.Kind != KindSettings || !settings.Present || string(settings.Data) != inventorySettingsData {
		t.Fatalf("settings source = %+v", settings)
	}
	rule := requireInventorySource(t, inventory, filepath.Join(home, ".gemini", "config", "rules", "baseline.md"))
	if rule.Kind != KindRule || string(rule.Data) != string(ruleData) || rule.SHA256 != task.ComputeSHA256(rule.Data) || rule.Size != int64(len(rule.Data)) {
		t.Fatalf("rule source = %+v", rule)
	}
	assertDefaultProjectBaseline(t, inventory, projectData)
	if inventory.TotalFiles < 5 || inventory.TotalBytes <= 0 || inventory.MembershipDigest == "" {
		t.Fatalf("inventory bounds/digest = files=%d bytes=%d digest=%q", inventory.TotalFiles, inventory.TotalBytes, inventory.MembershipDigest)
	}
	if !hasDirectory(t, inventory, filepath.Join(home, ".gemini", "config", "rules"), true) {
		t.Fatal("global rules directory was not snapshotted")
	}
}

const inventorySettingsData = `{"model":"provider-default","trustedWorkspaces":[]}`

func populateInventoryBaseline(t *testing.T, home, workspace string) ([]byte, []byte) {
	t.Helper()
	writeInventoryFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), []byte(inventorySettingsData))
	writeInventoryFile(t, filepath.Join(home, ".gemini", "config", "config.json"), []byte(`{"userSettings":{"remoteControlHostname":"opaque"}}`))
	writeInventoryFile(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"), nil)
	ruleData := []byte("Keep edits in the requested workspace.\n")
	writeInventoryFile(t, filepath.Join(home, ".gemini", "config", "rules", "baseline.md"), ruleData)
	writeInventoryFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "cache", DefaultProjectIDSource), []byte(defaultProjectID+"\n"))
	projectData := []byte(`{"id":"default-cli-project","name":"CLI Project","projectResources":{}}`)
	writeInventoryFile(t, filepath.Join(home, ".gemini", "config", "projects", "default-cli-project.json"), projectData)
	writeInventoryFile(t, filepath.Join(workspace, "AGENTS.md"), []byte("Use the workspace sentinel and keep sibling paths untouched.\n"))
	return projectData, ruleData
}

func assertInventoryRoots(t *testing.T, inventory PolicyInventory, home, workspace string) {
	t.Helper()
	if inventory.Home != home || inventory.Workspace != workspace {
		t.Fatalf("roots = %q, %q", inventory.Home, inventory.Workspace)
	}
}

func assertDefaultProjectBaseline(t *testing.T, inventory PolicyInventory, projectData []byte) {
	t.Helper()
	if inventory.Project.MappingPresent || inventory.Project.DefaultProjectID != defaultProjectID ||
		!inventory.Project.BaselineCandidate || !inventory.Project.RequiresLiveProof || len(inventory.Project.ProjectFiles) != 1 {
		t.Fatalf("project discovery = %+v", inventory.Project)
	}
	project := inventory.Project.ProjectFiles[0]
	if !project.MetadataValid || project.ID != defaultProjectID || !project.ProjectResourcesKnown || !project.ProjectResourcesEmpty {
		t.Fatalf("project metadata = %+v", project)
	}
	if source := requireInventorySource(t, inventory, project.Path); string(source.Data) != string(projectData) {
		t.Fatalf("project source bytes changed: %q", source.Data)
	}
}

func TestInventorySourcesAllowsMissingOptionalRoots(t *testing.T) {
	home, workspace := inventoryFixture(t)
	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Project.BaselineCandidate || !inventory.Project.RequiresLiveProof {
		t.Fatalf("missing project discovery = %+v", inventory.Project)
	}
	for _, path := range []string{
		filepath.Join(home, ".gemini", "config", "plugins"),
		filepath.Join(home, ".gemini", "config", "rules"),
		filepath.Join(workspace, ".agents"),
	} {
		if !hasDirectory(t, inventory, path, false) {
			t.Errorf("missing directory %s was not represented", path)
		}
	}
}

func TestInventorySourcesRetainsNonDefaultMappingForLiveProof(t *testing.T) {
	home, workspace := inventoryFixture(t)
	writeInventoryFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "cache", "projects.json"), []byte(`{"workspace":"other-project"}`))
	writeInventoryFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "cache", DefaultProjectIDSource), []byte(defaultProjectID))
	projectData := []byte(`{"id":"default-cli-project","name":"CLI Project","projectResources":{}}`)
	writeInventoryFile(t, filepath.Join(home, ".gemini", "config", "projects", "default-cli-project.json"), projectData)

	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	mapping := requireInventorySource(t, inventory, filepath.Join(home, ".gemini", "antigravity-cli", "cache", "projects.json"))
	if mapping.Kind != KindProjectMap || !mapping.Present || string(mapping.Data) != `{"workspace":"other-project"}` {
		t.Fatalf("mapping source = %+v", mapping)
	}
	if inventory.Project.BaselineCandidate || !inventory.Project.MappingPresent || !inventory.Project.RequiresLiveProof {
		t.Fatalf("mapped project discovery = %+v", inventory.Project)
	}
}

func TestInventorySourcesRejectsEnabledCustomization(t *testing.T) {
	cases := []struct {
		name string
		path func(home, workspace string) string
		data []byte
	}{
		{
			name: "global hooks",
			path: func(home, _ string) string { return filepath.Join(home, ".gemini", "config", "hooks.json") },
			data: []byte(`{"hooks":[]}`),
		},
		{
			name: "global mcp",
			path: func(home, _ string) string { return filepath.Join(home, ".gemini", "config", "mcp_config.json") },
			data: []byte(`{"mcpServers":{"server":{}}}`),
		},
		{
			name: "global plugin",
			path: func(home, _ string) string { return filepath.Join(home, ".gemini", "config", "plugins", "plugin.json") },
			data: []byte(`{}`),
		},
		{
			name: "global skill",
			path: func(home, _ string) string {
				return filepath.Join(home, ".gemini", "config", "skills", "skill", "SKILL.md")
			},
			data: []byte("skill"),
		},
		{
			name: "workspace hook",
			path: func(_ string, workspace string) string { return filepath.Join(workspace, ".agents", "hooks.json") },
			data: []byte(`{"hooks":[]}`),
		},
		{
			name: "workspace mcp",
			path: func(_ string, workspace string) string { return filepath.Join(workspace, ".agents", "mcp_config.json") },
			data: []byte(`{"mcpServers":{"server":{}}}`),
		},
		{
			name: "workspace workflow",
			path: func(_ string, workspace string) string { return filepath.Join(workspace, ".agents", "workflows.json") },
			data: []byte(`{"workflows":[]}`),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			home, workspace := inventoryFixture(t)
			writeInventoryFile(t, testCase.path(home, workspace), testCase.data)
			_, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
			if !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("enabled customization accepted: %v", err)
			}
		})
	}
}

func TestInventorySourcesRejectsUnsafeConfiguredEntries(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home, workspace string)
	}{
		{
			name: "rule symlink",
			setup: func(t *testing.T, home, _ string) {
				rules := filepath.Join(home, ".gemini", "config", "rules")
				if err := os.MkdirAll(filepath.Dir(rules), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), rules); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "project fifo",
			setup: func(t *testing.T, home, _ string) {
				projects := filepath.Join(home, ".gemini", "config", "projects")
				if err := os.MkdirAll(projects, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(filepath.Join(projects, "project.json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "workspace customization symlink",
			setup: func(t *testing.T, _, workspace string) {
				if err := os.Symlink(t.TempDir(), filepath.Join(workspace, ".agents")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			home, workspace := inventoryFixture(t)
			testCase.setup(t, home, workspace)
			_, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
			if !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("unsafe configured source accepted: %v", err)
			}
		})
	}
}

func TestInventorySourcesDetectsDirectoryMembershipChanges(t *testing.T) {
	home, workspace := inventoryFixture(t)
	first, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(workspace, ".agents", "rules"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if first.SameDirectoryMembership(second) {
		t.Fatal("new customization directories were not detected")
	}
	if membershipErr := first.CheckDirectoryMembership(second); !errors.Is(membershipErr, ErrInventoryChanged) {
		t.Fatalf("membership drift error = %v", membershipErr)
	}
	if !first.SameSourceDigests(second) {
		t.Fatal("empty customization tree unexpectedly changed source set")
	}
	writeInventoryFile(t, filepath.Join(workspace, ".agents", "rules", "local.md"), []byte("local rule"))
	third, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if second.SameDirectoryMembership(third) || second.SameSourceDigests(third) {
		t.Fatal("new rule source was not detected")
	}
}

func TestInventorySourcesEnforcesFiniteBounds(t *testing.T) {
	home, workspace := inventoryFixture(t)
	rules := filepath.Join(home, ".gemini", "config", "rules")
	if err := os.MkdirAll(rules, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= MaxInventoryEntries; index++ {
		writeInventoryFile(t, filepath.Join(rules, strings.Repeat("r", 3)+string(rune('a'+index%26))+"-"+itoa(index)+".md"), nil)
	}
	_, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if !errors.Is(err, ErrInventoryTooLarge) || !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("oversized directory accepted: %v", err)
	}
}

func TestInventorySourcesRejectsNonCanonicalRoots(t *testing.T) {
	home, workspace := inventoryFixture(t)
	alias := home + string(os.PathSeparator) + "."
	if _, err := InventorySources(InventoryRequest{Home: alias, Workspace: workspace}); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("noncanonical home accepted: %v", err)
	}
	link := filepath.Join(filepath.Dir(workspace), "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InventorySources(InventoryRequest{Home: home, Workspace: link}); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("symlink workspace accepted: %v", err)
	}
}

func TestPolicyDirectoryOpenUsesStableNoFollowDescriptor(t *testing.T) {
	root := sourceTestRoot(t)
	testStableDirectoryOpen(t, root)
	testRejectedDirectoryOpen(t, root)
	testReplacedDirectoryOpen(t, root)
}

func testStableDirectoryOpen(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, "candidate")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	file, err := openStablePolicyDirectory(directory, before)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	checkErr := checkPolicyDirectoryAfterRead(directory, file, opened, nil)
	if checkErr != nil {
		t.Fatal(checkErr)
	}
	closeErr := file.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func testRejectedDirectoryOpen(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, "candidate")
	link := filepath.Join(root, "directory-link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectoryOpenRejected(t, link, linkInfo, "symlink directory opened")

	fifo := filepath.Join(root, "directory-fifo")
	err = unix.Mkfifo(fifo, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fifoInfo, err := os.Lstat(fifo)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectoryOpenRejected(t, fifo, fifoInfo, "FIFO directory opened")
}

func testReplacedDirectoryOpen(t *testing.T, root string) {
	t.Helper()
	replaced := filepath.Join(root, "replaced")
	if err := os.Mkdir(replaced, 0o700); err != nil {
		t.Fatal(err)
	}
	replacedBefore, err := os.Lstat(replaced)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replaced, replaced+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, replaced); err != nil {
		t.Fatal(err)
	}
	assertDirectoryOpenRejected(t, replaced, replacedBefore, "replaced directory inode accepted")
}

func assertDirectoryOpenRejected(t *testing.T, path string, before os.FileInfo, message string) {
	t.Helper()
	if _, err := openStablePolicyDirectory(path, before); err == nil {
		t.Fatal(message)
	}
}

func TestPolicyDirectoryDetectsMutationAfterRead(t *testing.T) {
	root := sourceTestRoot(t)
	directory := filepath.Join(root, "candidate")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	file, err := openStablePolicyDirectory(directory, before)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	writeErr := os.WriteFile(filepath.Join(directory, "new-entry"), []byte("changed"), 0o600)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	checkErr := checkPolicyDirectoryAfterRead(directory, file, opened, nil)
	if checkErr == nil {
		t.Fatal("directory mutation accepted")
	}
}

func inventoryFixture(t *testing.T) (string, string) {
	t.Helper()
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, workspace
}

func writeInventoryFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireInventorySource(t *testing.T, inventory PolicyInventory, path string) InventorySource {
	t.Helper()
	for _, source := range inventory.Sources {
		if source.Path == path {
			return source
		}
	}
	t.Fatalf("source %s missing", path)
	return InventorySource{}
}

func hasDirectory(t *testing.T, inventory PolicyInventory, path string, present bool) bool {
	t.Helper()
	for _, directory := range inventory.Directories {
		if directory.Path == path {
			return directory.Present == present
		}
	}
	return false
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
