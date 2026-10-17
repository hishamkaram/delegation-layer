package contract

// AnchorRecord pins the workspace's private parent before Git modifies the
// repository. Its symlink marker points at this journal, not a second owner.
type AnchorRecord struct {
	SchemaVersion int               `json:"schema_version"`
	WorkspaceID   string            `json:"workspace_id"`
	Path          string            `json:"path"`
	Identity      DirectoryIdentity `json:"identity"`
}
