package workspacefiles

import (
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
)

type directoryCursor struct {
	SchemaVersion int    `json:"schema_version"`
	Selection     string `json:"selection"`
	Version       string `json:"version"`
	AfterBase64   string `json:"after_base64"`
}

func decodeCursor(encoded, selection, version string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if len(encoded) > 16384 {
		return "", failure(InvalidRequest, "cursor", nil)
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return "", failure(InvalidRequest, "cursor", err)
	}
	var cursor directoryCursor
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cursor); err != nil {
		return "", failure(InvalidRequest, "cursor", err)
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", failure(InvalidRequest, "cursor", err)
	}
	if cursor.SchemaVersion != SchemaVersion || cursor.Selection != selection {
		return "", failure(InvalidRequest, "cursor", nil)
	}
	if cursor.Version != version {
		return "", failure(Changed, "cursor", nil)
	}
	after, err := base64.StdEncoding.Strict().DecodeString(cursor.AfterBase64)
	if err != nil || len(after) == 0 || len(after) > 4096 || strings.ContainsAny(string(after), "/\x00") || string(after) == "." || string(after) == ".." {
		return "", failure(InvalidRequest, "cursor", err)
	}
	return string(after), nil
}

func encodeCursor(selection, version, after string) (string, error) {
	data, err := json.Marshal(directoryCursor{
		SchemaVersion: SchemaVersion, Selection: selection,
		Version: version, AfterBase64: base64.StdEncoding.EncodeToString([]byte(after)),
	})
	if err != nil {
		return "", failure(Changed, "cursor", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// nameHeap keeps the largest retained name at its root. At most page size + 1
// names are retained, independent of total directory size.
type nameHeap []string

func (h nameHeap) Len() int           { return len(h) }
func (h nameHeap) Less(i, j int) bool { return h[i] > h[j] }
func (h nameHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *nameHeap) Push(value any) {
	// container/heap calls Push only with the string names supplied below.
	name, ok := value.(string)
	if !ok {
		panic("workspacefiles: non-string directory heap value")
	}
	*h = append(*h, name)
}

func (h *nameHeap) Pop() any {
	last := len(*h) - 1
	value := (*h)[last]
	(*h)[last] = ""
	*h = (*h)[:last]
	return value
}

func scanNames(ctx context.Context, file *os.File, after string, count int) ([]string, error) {
	selected := make(nameHeap, 0, count)
	for {
		if err := ctx.Err(); err != nil {
			return nil, classify("directory_scan", err)
		}
		batch, err := file.Readdirnames(128)
		for _, name := range batch {
			if name <= after {
				continue
			}
			if len(selected) < count {
				heap.Push(&selected, name)
			} else if name < selected[0] {
				selected[0] = name
				heap.Fix(&selected, 0)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, classify("directory_scan", err)
		}
	}
	slices.Sort(selected)
	return []string(selected), nil
}

func (s *selection) list(ctx context.Context, path string, request Request) (_ *ListResult, resultErr error) {
	directory, err := s.root.OpenRoot(path)
	if err != nil {
		return nil, classify("directory_open", err)
	}
	defer func() { resultErr = errors.Join(resultErr, classify("directory_close", directory.Close())) }()
	file, err := directory.Open(".")
	if err != nil {
		return nil, classify("directory_open", err)
	}
	defer func() { resultErr = errors.Join(resultErr, classify("directory_close", file.Close())) }()
	version, err := fileVersion(file)
	if err != nil {
		return nil, err
	}
	if err = requireVersion(request.ExpectedVersion, version); err != nil {
		return nil, err
	}
	selection := digest(s.id + "\x00" + request.ExpectedRoot + "\x00" + path)
	after, err := decodeCursor(request.Cursor, selection, version)
	if err != nil {
		return nil, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = MaxEntries
	}
	names, err := scanNames(ctx, file, after, limit+1)
	if err != nil {
		return nil, err
	}
	result := &ListResult{Entries: make([]Entry, 0, min(limit, len(names))), Version: version, EOF: len(names) <= limit}
	if !result.EOF {
		names = names[:limit]
		result.NextCursor, err = encodeCursor(selection, version, names[len(names)-1])
		if err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		entry, entryErr := s.listEntry(ctx, directory, path, name)
		if entryErr != nil {
			return nil, entryErr
		}
		result.Entries = append(result.Entries, entry)
	}
	afterVersion, err := fileVersion(file)
	if err != nil {
		return nil, err
	}
	if afterVersion != version {
		return nil, failure(Changed, "directory_scan", nil)
	}
	return result, nil
}

func (s *selection) listEntry(ctx context.Context, directory *os.Root, path, name string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, classify("directory_scan", err)
	}
	info, err := directory.Lstat(name)
	if err != nil {
		// A vanished name makes the whole live page incomplete, never EOF.
		return Entry{}, failure(Changed, "directory_scan", err)
	}
	entry, err := makeEntry(childPath(path, name), info)
	if err == nil && info.IsDir() {
		entry.Git = s.gitMarker(childPath(path, name))
	}
	return entry, err
}

func fileVersion(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", classify("metadata", err)
	}
	return metadataVersion(info)
}
