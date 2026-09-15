package antigravity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// InventorySources bounds the amount of provider configuration that can be
// admitted into one transient resolver input. The bytes are returned to the
// caller for parsing and are never written by this package.
const (
	MaxInventoryFiles      = 256
	MaxInventoryBytes      = 8 * task.MaxControlRecordSize
	MaxInventoryEntries    = 256
	MaxWorkspaceAncestors  = 128
	DefaultProjectIDSource = "default_project_id.txt"
)

var (
	// ErrInventoryTooLarge means that the bounded source or directory walk
	// cannot safely describe the provider's effective configuration.
	ErrInventoryTooLarge = errors.New("antigravity policy inventory exceeds bound")
	// ErrInventoryChanged is reserved for a caller comparing two snapshots.
	ErrInventoryChanged = errors.New("antigravity policy inventory changed")
)

var (
	globalConfigEntries = map[string]struct{}{
		".migrated": {}, "AGENTS.md": {}, "GEMINI.md": {}, "config.json": {},
		"hooks.json": {}, "mcp_config.json": {}, "workflows.json": {},
		"agents": {}, "global_workflows": {}, "plugins": {}, "projects": {},
		"rules": {}, "skills": {}, "workflows": {},
	}
	workspaceCustomizationEntries = map[string]struct{}{
		"AGENTS.md": {}, "GEMINI.md": {}, "agents": {}, "hooks.json": {},
		"mcp_config.json": {}, "plugins": {}, "rules": {}, "skills": {},
		"skills.json": {}, "workflows": {}, "workflows.json": {},
	}
)

// InventoryRequest supplies the already-canonical paths used by the
// provider's documented global and workspace discovery roots. The resolver
// deliberately does not consult HOME, environment variables, authentication
// state, or a subprocess to derive either path.
type InventoryRequest struct {
	Home      string
	Workspace string
}

// SourceScope identifies the discovery layer that made a source applicable.
type SourceScope string

const (
	ScopeGlobal    SourceScope = "global"
	ScopeProject   SourceScope = "project"
	ScopeWorkspace SourceScope = "workspace"
)

// SourceKind identifies the bounded, non-secret source category. A rule is
// retained as context and hashed; extension, hook, and MCP sources are
// conservatively required to be absent or empty by the inventory walk.
type SourceKind string

const (
	KindSettings      SourceKind = "settings"
	KindSharedConfig  SourceKind = "shared-config"
	KindProjectMap    SourceKind = "project-mapping"
	KindDefaultID     SourceKind = "default-project-id"
	KindProject       SourceKind = "project"
	KindRule          SourceKind = "rule"
	KindHook          SourceKind = "hook"
	KindMCP           SourceKind = "mcp"
	KindPlugin        SourceKind = "plugin"
	KindAgent         SourceKind = "agent"
	KindSkill         SourceKind = "skill"
	KindWorkflow      SourceKind = "workflow"
	KindKeybindings   SourceKind = "keybindings"
	KindMetadata      SourceKind = "metadata"
	KindCustomization SourceKind = "customization"
)

// InventorySource is one exact source decision. Data is transient resolver
// input; callers must parse or digest it before releasing the inventory and
// must not persist credentials or provider runtime state from it.
type InventorySource struct {
	Path    string
	Scope   SourceScope
	Kind    SourceKind
	Present bool
	Size    int64
	SHA256  string
	Data    []byte
}

// DirectoryEntry is a no-follow membership record. Symlinks and special
// files are rejected before they can become a policy source.
type DirectoryEntry struct {
	Name string
	Kind string
}

// DirectorySnapshot records the exact membership of a configured discovery
// directory. A missing candidate is represented explicitly so that a later
// re-enumeration catches a newly created policy tree.
type DirectorySnapshot struct {
	Path    string
	Scope   SourceScope
	Kind    SourceKind
	Present bool
	Entries []DirectoryEntry
	SHA256  string
}

// ProjectFile is the safe metadata subset observed in one project JSON file.
// The original bytes remain in Sources for the root-owned project parser.
type ProjectFile struct {
	Path                  string
	ID                    string
	Name                  string
	MetadataValid         bool
	ProjectResourcesKnown bool
	ProjectResourcesEmpty bool
}

// ProjectDiscovery describes project selection inputs without claiming that
// a project JSON file is a complete permission report.
type ProjectDiscovery struct {
	MappingPath             string
	MappingPresent          bool
	DefaultProjectIDPath    string
	DefaultProjectIDPresent bool
	DefaultProjectIDKnown   bool
	DefaultProjectID        string
	ProjectFiles            []ProjectFile
	BaselineCandidate       bool
	RequiresLiveProof       bool
}

// PolicyInventory is a bounded, deterministic snapshot of the source paths
// that the Antigravity CLI can expose. It is an input to a later
// policy parser and live capability gate, not a substitute for per-task checks.
type PolicyInventory struct {
	Home             string
	Workspace        string
	Sources          []InventorySource
	Directories      []DirectorySnapshot
	Project          ProjectDiscovery
	TotalFiles       int
	TotalBytes       int64
	MembershipDigest string
}

// SameDirectoryMembership compares only directory identities and entries.
// Source bytes are intentionally excluded so callers can separately decide
// whether a changed file digest invalidates an otherwise stable membership.
func (i PolicyInventory) SameDirectoryMembership(other PolicyInventory) bool {
	if i.Home != other.Home || i.Workspace != other.Workspace || len(i.Directories) != len(other.Directories) {
		return false
	}
	for index := range i.Directories {
		left, right := i.Directories[index], other.Directories[index]
		if left.Path != right.Path || left.Scope != right.Scope || left.Kind != right.Kind ||
			left.Present != right.Present || left.SHA256 != right.SHA256 || len(left.Entries) != len(right.Entries) {
			return false
		}
		for entryIndex := range left.Entries {
			if left.Entries[entryIndex] != right.Entries[entryIndex] {
				return false
			}
		}
	}
	return true
}

// CheckDirectoryMembership returns ErrInventoryChanged when a re-enumerated
// inventory has a different configured directory or membership.
func (i PolicyInventory) CheckDirectoryMembership(other PolicyInventory) error {
	if !i.SameDirectoryMembership(other) {
		return ErrInventoryChanged
	}
	return nil
}

// SameSourceDigests compares source identity and content metadata while
// deliberately ignoring transient Data bytes.
func (i PolicyInventory) SameSourceDigests(other PolicyInventory) bool {
	if i.Home != other.Home || i.Workspace != other.Workspace || len(i.Sources) != len(other.Sources) {
		return false
	}
	for index := range i.Sources {
		left, right := i.Sources[index], other.Sources[index]
		if left.Path != right.Path || left.Scope != right.Scope || left.Kind != right.Kind ||
			left.Present != right.Present || left.Size != right.Size || left.SHA256 != right.SHA256 {
			return false
		}
	}
	return true
}

type inventoryBuilder struct {
	inventory     PolicyInventory
	sourceKinds   map[string]SourceKind
	directoryKeys map[string]struct{}
	files         int
	bytes         int64
	entries       int
}

// InventorySources reads the fixed, observed Antigravity source roots. Every
// file is opened through readPolicySource, which enforces canonical no-follow
// stable reads and the per-file control-record bound.
func InventorySources(request InventoryRequest) (PolicyInventory, error) {
	home, err := validateInventoryRoot(request.Home, "home")
	if err != nil {
		return PolicyInventory{}, err
	}
	workspace, err := validateInventoryRoot(request.Workspace, "workspace")
	if err != nil {
		return PolicyInventory{}, err
	}
	builder := inventoryBuilder{
		inventory:     PolicyInventory{Home: home, Workspace: workspace},
		sourceKinds:   make(map[string]SourceKind),
		directoryKeys: make(map[string]struct{}),
	}
	if err := builder.collectGlobal(home); err != nil {
		return PolicyInventory{}, err
	}
	if err := builder.collectWorkspace(workspace); err != nil {
		return PolicyInventory{}, err
	}
	builder.inventory.TotalFiles = builder.files
	builder.inventory.TotalBytes = builder.bytes
	sort.Slice(builder.inventory.Sources, func(left, right int) bool {
		return builder.inventory.Sources[left].Path < builder.inventory.Sources[right].Path
	})
	sort.Slice(builder.inventory.Directories, func(left, right int) bool {
		return builder.inventory.Directories[left].Path < builder.inventory.Directories[right].Path
	})
	builder.inventory.MembershipDigest = membershipDigest(builder.inventory.Directories)
	return builder.inventory, nil
}

func validateInventoryRoot(path, label string) (string, error) {
	if path == "" || filepath.Clean(path) != path || !filepath.IsAbs(path) {
		return "", inventoryError("%s must be an absolute canonical directory", label)
	}
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path {
		return "", inventoryError("%s is not canonical: %s", label, path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", inventoryError("cannot inspect %s: %v", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", inventoryError("%s is not a real directory: %s", label, path)
	}
	return path, nil
}

func (b *inventoryBuilder) collectGlobal(home string) error {
	gemini := filepath.Join(home, ".gemini")
	antigravity := filepath.Join(gemini, "antigravity-cli")
	configRoot := filepath.Join(gemini, "config")

	for _, source := range []struct {
		path  string
		scope SourceScope
		kind  SourceKind
	}{
		{filepath.Join(antigravity, "settings.json"), ScopeGlobal, KindSettings},
		{filepath.Join(antigravity, "keybindings.json"), ScopeGlobal, KindKeybindings},
		{filepath.Join(antigravity, "cache", "projects.json"), ScopeProject, KindProjectMap},
		{filepath.Join(antigravity, "cache", DefaultProjectIDSource), ScopeProject, KindDefaultID},
		{filepath.Join(configRoot, "config.json"), ScopeGlobal, KindSharedConfig},
		{filepath.Join(configRoot, "mcp_config.json"), ScopeGlobal, KindMCP},
		{filepath.Join(configRoot, "hooks.json"), ScopeGlobal, KindHook},
		{filepath.Join(configRoot, "workflows.json"), ScopeGlobal, KindWorkflow},
		{filepath.Join(configRoot, ".migrated"), ScopeGlobal, KindMetadata},
		{filepath.Join(gemini, "GEMINI.md"), ScopeGlobal, KindRule},
		{filepath.Join(configRoot, "GEMINI.md"), ScopeGlobal, KindRule},
		{filepath.Join(configRoot, "AGENTS.md"), ScopeGlobal, KindRule},
	} {
		if err := b.addSourceFile(source.path, source.scope, source.kind); err != nil {
			return err
		}
	}
	mappingPath := filepath.Join(antigravity, "cache", "projects.json")
	mapping, err := b.sourceAt(mappingPath)
	if err != nil {
		return err
	}
	b.inventory.Project.MappingPath = mappingPath
	b.inventory.Project.MappingPresent = mapping.Present
	defaultIDPath := filepath.Join(antigravity, "cache", DefaultProjectIDSource)
	defaultID, err := b.sourceAt(defaultIDPath)
	if err != nil {
		return err
	}
	b.inventory.Project.DefaultProjectIDPath = defaultIDPath
	b.inventory.Project.DefaultProjectIDPresent = defaultID.Present
	if defaultID.Present {
		value := strings.TrimSpace(string(defaultID.Data))
		if value != "" && !strings.ContainsRune(value, '\x00') && utf8.ValidString(value) {
			b.inventory.Project.DefaultProjectIDKnown = true
			b.inventory.Project.DefaultProjectID = value
		}
	}
	if err := b.requireEmptySource(filepath.Join(configRoot, "mcp_config.json")); err != nil {
		return err
	}
	if err := b.requireEmptySource(filepath.Join(configRoot, "hooks.json")); err != nil {
		return err
	}
	if err := b.requireEmptySource(filepath.Join(configRoot, "workflows.json")); err != nil {
		return err
	}
	if err := b.collectGlobalConfig(configRoot); err != nil {
		return err
	}
	return b.collectExtensionDirectory(filepath.Join(antigravity, "plugins"), ScopeGlobal, KindPlugin)
}

func (b *inventoryBuilder) collectGlobalConfig(configRoot string) error {
	snapshot, err := b.inspectDirectory(configRoot, ScopeGlobal, KindSharedConfig)
	if err != nil {
		return err
	}
	if snapshot.Present {
		for _, entry := range snapshot.Entries {
			if _, ok := globalConfigEntries[entry.Name]; !ok {
				return inventoryError("unknown global customization entry %s", filepath.Join(configRoot, entry.Name))
			}
		}
	}
	for _, directory := range []struct {
		name  string
		kind  SourceKind
		style string
	}{
		{"rules", KindRule, "rules"},
		{"agents", KindAgent, "extension"},
		{"skills", KindSkill, "extension"},
		{"plugins", KindPlugin, "extension"},
		{"global_workflows", KindWorkflow, "extension"},
		{"workflows", KindWorkflow, "extension"},
	} {
		path := filepath.Join(configRoot, directory.name)
		if directory.style == "rules" {
			if err := b.collectRuleDirectory(path, ScopeGlobal, directory.kind); err != nil {
				return err
			}
			continue
		}
		if err := b.collectExtensionDirectory(path, ScopeGlobal, directory.kind); err != nil {
			return err
		}
	}
	return b.collectProjectDirectory(filepath.Join(configRoot, "projects"))
}

func (b *inventoryBuilder) collectWorkspace(workspace string) error {
	current := workspace
	for depth := 0; ; depth++ {
		if depth >= MaxWorkspaceAncestors {
			return inventoryTooLarge("workspace ancestor depth")
		}
		for _, name := range []string{"GEMINI.md", "AGENTS.md"} {
			if err := b.addSourceFile(filepath.Join(current, name), ScopeWorkspace, KindRule); err != nil {
				return err
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for _, name := range []string{".agents", ".agent", "_agents", "_agent"} {
		if err := b.collectCustomizationDirectory(filepath.Join(workspace, name)); err != nil {
			return err
		}
	}
	return nil
}

func (b *inventoryBuilder) collectCustomizationDirectory(path string) error {
	snapshot, err := b.inspectDirectory(path, ScopeWorkspace, KindCustomization)
	if err != nil {
		return err
	}
	if !snapshot.Present {
		return nil
	}
	for _, entry := range snapshot.Entries {
		if _, ok := workspaceCustomizationEntries[entry.Name]; !ok {
			return inventoryError("unknown workspace customization entry %s", filepath.Join(path, entry.Name))
		}
		if err := b.collectCustomizationEntry(path, entry.Name); err != nil {
			return err
		}
	}
	return nil
}

func (b *inventoryBuilder) collectCustomizationEntry(path, name string) error {
	entryPath := filepath.Join(path, name)
	switch name {
	case "AGENTS.md", "GEMINI.md":
		return b.addSourceFile(entryPath, ScopeWorkspace, KindRule)
	case "hooks.json":
		return b.addEmptySource(entryPath, ScopeWorkspace, KindHook)
	case "mcp_config.json":
		return b.addEmptySource(entryPath, ScopeWorkspace, KindMCP)
	case "rules":
		return b.collectRuleDirectory(entryPath, ScopeWorkspace, KindRule)
	case "agents":
		return b.collectExtensionDirectory(entryPath, ScopeWorkspace, KindAgent)
	case "skills":
		return b.collectExtensionDirectory(entryPath, ScopeWorkspace, KindSkill)
	case "plugins":
		return b.collectExtensionDirectory(entryPath, ScopeWorkspace, KindPlugin)
	case "workflows":
		return b.collectExtensionDirectory(entryPath, ScopeWorkspace, KindWorkflow)
	case "skills.json":
		return b.addEmptySource(entryPath, ScopeWorkspace, KindSkill)
	case "workflows.json":
		return b.addEmptySource(entryPath, ScopeWorkspace, KindWorkflow)
	default:
		return inventoryError("unknown workspace customization entry %s", entryPath)
	}
}

func (b *inventoryBuilder) collectProjectDirectory(path string) error {
	snapshot, err := b.inspectDirectory(path, ScopeProject, KindProject)
	if err != nil {
		return err
	}
	if !snapshot.Present {
		b.inventory.Project.ProjectFiles = nil
		b.inventory.Project.BaselineCandidate = false
		b.inventory.Project.RequiresLiveProof = true
		return nil
	}
	for _, entry := range snapshot.Entries {
		if entry.Kind != "file" || strings.ToLower(filepath.Ext(entry.Name)) != ".json" {
			return inventoryError("project directory contains unsupported entry %s", filepath.Join(path, entry.Name))
		}
		entryPath := filepath.Join(path, entry.Name)
		source, err := b.addSourceFileValue(entryPath, ScopeProject, KindProject)
		if err != nil {
			return err
		}
		metadata, err := parseProjectMetadata(entryPath, source.Data)
		if err != nil {
			return err
		}
		b.inventory.Project.ProjectFiles = append(b.inventory.Project.ProjectFiles, metadata)
	}
	b.inventory.Project.RequiresLiveProof = true
	b.inventory.Project.BaselineCandidate = b.isDefaultProjectCandidate()
	return nil
}

func (b *inventoryBuilder) isDefaultProjectCandidate() bool {
	project := &b.inventory.Project
	if project.MappingPresent || !project.DefaultProjectIDPresent || !project.DefaultProjectIDKnown ||
		project.DefaultProjectID != defaultProjectID || len(project.ProjectFiles) != 1 {
		return false
	}
	file := project.ProjectFiles[0]
	return file.MetadataValid && file.ID == defaultProjectID && file.ProjectResourcesKnown && file.ProjectResourcesEmpty
}

func parseProjectMetadata(path string, data []byte) (ProjectFile, error) {
	metadata := ProjectFile{Path: path}
	if !utf8.Valid(data) {
		return metadata, inventoryError("project JSON is not UTF-8: %s", path)
	}
	if err := task.ValidateJSONStructure(data); err != nil {
		return metadata, inventoryError("project JSON is not a bounded object: %s", path)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return metadata, inventoryError("project JSON root is not an object: %s", path)
	}
	metadata.MetadataValid = hasProjectMetadataFields(fields)
	if raw, ok := fields["id"]; ok {
		metadata.MetadataValid = parseProjectString(raw, &metadata.ID) && metadata.MetadataValid
	}
	if raw, ok := fields["name"]; ok {
		metadata.MetadataValid = parseProjectString(raw, &metadata.Name) && metadata.MetadataValid
	}
	if raw, ok := fields["projectResources"]; ok {
		known, empty, valid := parseProjectResources(raw)
		metadata.ProjectResourcesKnown, metadata.ProjectResourcesEmpty = known, empty
		metadata.MetadataValid = valid && metadata.MetadataValid
	}
	return metadata, nil
}

func hasProjectMetadataFields(fields map[string]json.RawMessage) bool {
	for _, required := range []string{"id", "name", "projectResources"} {
		if _, ok := fields[required]; !ok {
			return false
		}
	}
	return true
}

func parseProjectString(raw json.RawMessage, target *string) bool {
	if err := json.Unmarshal(raw, target); err != nil {
		return false
	}
	return strings.TrimSpace(*target) != "" && !strings.ContainsRune(*target, '\x00')
}

func parseProjectResources(raw json.RawMessage) (known, empty, valid bool) {
	var resources map[string]json.RawMessage
	if err := json.Unmarshal(raw, &resources); err != nil || resources == nil {
		return false, false, false
	}
	return true, len(resources) == 0, true
}

func (b *inventoryBuilder) collectRuleDirectory(path string, scope SourceScope, kind SourceKind) error {
	snapshot, err := b.inspectDirectory(path, scope, kind)
	if err != nil {
		return err
	}
	if !snapshot.Present {
		return nil
	}
	for _, entry := range snapshot.Entries {
		entryPath := filepath.Join(path, entry.Name)
		switch entry.Kind {
		case "directory":
			if err := b.collectRuleDirectory(entryPath, scope, kind); err != nil {
				return err
			}
		case "file":
			if filepath.Ext(entry.Name) != ".md" {
				return inventoryError("rule directory contains non-Markdown source %s", entryPath)
			}
			if err := b.addSourceFile(entryPath, scope, kind); err != nil {
				return err
			}
		default:
			return inventoryError("rule directory contains unsupported entry %s", entryPath)
		}
	}
	return nil
}

func (b *inventoryBuilder) collectExtensionDirectory(path string, scope SourceScope, kind SourceKind) error {
	snapshot, err := b.inspectDirectory(path, scope, kind)
	if err != nil {
		return err
	}
	if snapshot.Present && len(snapshot.Entries) != 0 {
		return inventoryError("nonempty %s customization tree: %s", kind, path)
	}
	return nil
}

func (b *inventoryBuilder) addSourceFile(path string, scope SourceScope, kind SourceKind) error {
	_, err := b.addSourceFileValue(path, scope, kind)
	return err
}

func (b *inventoryBuilder) addEmptySource(path string, scope SourceScope, kind SourceKind) error {
	if err := b.addSourceFile(path, scope, kind); err != nil {
		return err
	}
	return b.requireEmptySource(path)
}

func (b *inventoryBuilder) sourceAt(path string) (InventorySource, error) {
	for _, source := range b.inventory.Sources {
		if source.Path == path {
			return source, nil
		}
	}
	return InventorySource{}, inventoryError("source was not enumerated: %s", path)
}

func (b *inventoryBuilder) addSourceFileValue(path string, scope SourceScope, kind SourceKind) (InventorySource, error) {
	raw, err := readPolicySource(path)
	if err != nil {
		return InventorySource{}, err
	}
	source := InventorySource{
		Path: path, Scope: scope, Kind: kind, Present: raw.Present,
		Data: raw.Data,
	}
	if raw.Present {
		source.Size = int64(len(raw.Data))
		source.SHA256 = task.ComputeSHA256(raw.Data)
		if b.files >= MaxInventoryFiles {
			return InventorySource{}, inventoryTooLarge("source file count")
		}
		if b.bytes > MaxInventoryBytes-source.Size {
			return InventorySource{}, inventoryTooLarge("source bytes")
		}
		b.files++
		b.bytes += source.Size
	}
	if previous, exists := b.sourceKinds[path]; exists {
		if previous != kind {
			return InventorySource{}, inventoryError("source has conflicting categories: %s", path)
		}
		return InventorySource{}, inventoryError("source was enumerated twice: %s", path)
	}
	b.sourceKinds[path] = kind
	b.inventory.Sources = append(b.inventory.Sources, source)
	return source, nil
}

func (b *inventoryBuilder) requireEmptySource(path string) error {
	for _, source := range b.inventory.Sources {
		if source.Path == path && source.Present && len(source.Data) != 0 {
			return inventoryError("enabled customization source is unsupported: %s", path)
		}
	}
	return nil
}

func (b *inventoryBuilder) inspectDirectory(path string, scope SourceScope, kind SourceKind) (snapshot DirectorySnapshot, returnErr error) {
	if err := validateDirectoryPath(path); err != nil {
		return DirectorySnapshot{}, err
	}
	if _, exists := b.directoryKeys[path]; exists {
		return DirectorySnapshot{}, inventoryError("directory was enumerated twice: %s", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		missing := DirectorySnapshot{Path: path, Scope: scope, Kind: kind}
		b.directoryKeys[path] = struct{}{}
		b.inventory.Directories = append(b.inventory.Directories, missing)
		return missing, nil
	}
	if err != nil {
		return DirectorySnapshot{}, inventoryError("cannot inspect directory %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return DirectorySnapshot{}, inventoryError("configured path is not a real directory: %s", path)
	}
	entries, err := readStableDirectoryEntries(path, info)
	if err != nil {
		return DirectorySnapshot{}, err
	}
	snapshot = DirectorySnapshot{Path: path, Scope: scope, Kind: kind, Present: true, Entries: entries}
	if b.entries > MaxInventoryEntries-len(snapshot.Entries) {
		return DirectorySnapshot{}, inventoryTooLarge("total directory entry count")
	}
	b.entries += len(snapshot.Entries)
	snapshot.SHA256 = directoryDigest(snapshot.Entries)
	b.directoryKeys[path] = struct{}{}
	b.inventory.Directories = append(b.inventory.Directories, snapshot)
	return snapshot, nil
}

func validateDirectoryPath(path string) error {
	if filepath.Clean(path) != path || !filepath.IsAbs(path) {
		return inventoryError("directory is not an absolute canonical path: %s", path)
	}
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path {
		return inventoryError("directory is not canonical: %s", path)
	}
	return nil
}

func readStableDirectoryEntries(path string, before os.FileInfo) (result []DirectoryEntry, returnErr error) {
	file, err := openStablePolicyDirectory(path, before)
	if err != nil {
		return nil, inventoryError("cannot open configured directory %s: %v", path, err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	entries, err := file.ReadDir(MaxInventoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, inventoryError("cannot enumerate configured directory %s: %v", path, err)
	}
	if len(entries) > MaxInventoryEntries {
		return nil, inventoryTooLarge("directory entry count")
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	result = make([]DirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		entryKind, err := classifyDirectoryEntry(entry)
		if err != nil {
			return nil, inventoryError("unsafe configured entry %s: %v", filepath.Join(path, entry.Name()), err)
		}
		result = append(result, DirectoryEntry{Name: entry.Name(), Kind: entryKind})
	}
	if err := checkPolicyDirectoryAfterRead(path, file, before); err != nil {
		return nil, inventoryError("configured directory changed during read %s: %v", path, err)
	}
	return result, returnErr
}

func openStablePolicyDirectory(path string, before os.FileInfo) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, errors.Join(errors.New("invalid policy directory descriptor"), unix.Close(fd))
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, errors.Join(errors.New("policy directory changed before read"), file.Close())
	}
	return file, nil
}

func checkPolicyDirectoryAfterRead(path string, file *os.File, opened os.FileInfo) error {
	final, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !final.IsDir() || !named.IsDir() || !os.SameFile(final, opened) || !os.SameFile(named, opened) ||
		final.Size() != opened.Size() || !final.ModTime().Equal(opened.ModTime()) {
		return errors.New("policy directory changed during read")
	}
	return nil
}

func classifyDirectoryEntry(entry os.DirEntry) (string, error) {
	mode := entry.Type()
	if mode&os.ModeSymlink != 0 {
		return "", errors.New("symlink is not an admissible source")
	}
	if mode.IsDir() {
		return "directory", nil
	}
	if mode.IsRegular() {
		return "file", nil
	}
	info, err := entry.Info()
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("symlink is not an admissible source")
	}
	if info.Mode().IsDir() {
		return "directory", nil
	}
	if info.Mode().IsRegular() {
		return "file", nil
	}
	return "", errors.New("nonregular source")
}

func directoryDigest(entries []DirectoryEntry) string {
	var encoded strings.Builder
	for _, entry := range entries {
		encoded.WriteString(entry.Name)
		encoded.WriteByte(0)
		encoded.WriteString(entry.Kind)
		encoded.WriteByte(0)
	}
	return task.ComputeSHA256([]byte(encoded.String()))
}

func membershipDigest(directories []DirectorySnapshot) string {
	var encoded strings.Builder
	for _, directory := range directories {
		encoded.WriteString(directory.Path)
		encoded.WriteByte(0)
		encoded.WriteString(string(directory.Scope))
		encoded.WriteByte(0)
		encoded.WriteString(string(directory.Kind))
		encoded.WriteByte(0)
		if directory.Present {
			encoded.WriteString("present")
		} else {
			encoded.WriteString("absent")
		}
		encoded.WriteByte(0)
		encoded.WriteString(directory.SHA256)
		encoded.WriteByte('\n')
	}
	return task.ComputeSHA256([]byte(encoded.String()))
}

func inventoryError(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedProfile, fmt.Sprintf(format, arguments...))
}

func inventoryTooLarge(reason string) error {
	return fmt.Errorf("%w: %w: %s", ErrUnsupportedProfile, ErrInventoryTooLarge, reason)
}
