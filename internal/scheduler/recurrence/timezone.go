package recurrence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

// ResolveTimezone resolves an explicit IANA name, or snapshots the native
// default when explicit is empty. It never returns Local or invents an offset.
func ResolveTimezone(explicit string) (string, error) {
	if explicit != "" {
		_, err := namedLocation(explicit)
		return explicit, err
	}
	tz, present := os.LookupEnv("TZ")
	return resolveLocalTimezone(localZoneSource{
		tz: tz, tzPresent: present, localtime: "/etc/localtime",
		roots: []string{"/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ", "/etc/zoneinfo", "/var/db/timezone/zoneinfo"},
	})
}

func namedLocation(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > contract.MaxOpaqueTextBytes || strings.ContainsFunc(name, unicode.IsSpace) {
		return nil, fmt.Errorf("%w: supply a nonempty named timezone other than Local", ErrInvalidTimezone)
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTimezone, err)
	}
	return location, nil
}

type localZoneSource struct {
	tz        string
	tzPresent bool
	localtime string
	roots     []string
}

func resolveLocalTimezone(source localZoneSource) (string, error) {
	if !source.tzPresent {
		return zoneNameFromPath(source.localtime, source.roots)
	}
	if source.tz == "" {
		return "UTC", nil
	}
	zone := strings.TrimPrefix(source.tz, ":")
	if zone == "" || zone == "UTC" {
		return "UTC", nil
	}
	if filepath.IsAbs(zone) {
		return zoneNameFromPath(zone, source.roots)
	}
	if _, err := namedLocation(zone); err != nil {
		return "", fmt.Errorf("%w: TZ: %w", ErrLocalTimezone, err)
	}
	return zone, nil
}

func zoneNameFromPath(path string, roots []string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrLocalTimezone, err)
	}
	target, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrLocalTimezone, err)
	}
	for _, root := range roots {
		name, found, err := zoneNameInRoot(path, resolved, target, root)
		if err != nil {
			return "", err
		}
		if found {
			return name, nil
		}
	}
	return "", fmt.Errorf("%w: timezone file has no established zoneinfo name", ErrLocalTimezone)
}

func zoneNameInRoot(path, resolved string, target os.FileInfo, root string) (string, bool, error) {
	physicalRoot, err := filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrLocalTimezone, err)
	}
	relative, contained := relativeZonePath(physicalRoot, resolved)
	if !contained {
		return "", false, nil
	}
	// Preserve a valid alias spelled beneath the known root. The physical-path
	// containment above still prevents a symlink from escaping that root.
	if alias, inside := relativeZonePath(root, path); inside {
		relative = alias
	}
	name := filepath.ToSlash(relative)
	if _, zoneErr := namedLocation(name); zoneErr != nil {
		return "", false, fmt.Errorf("%w: %w", ErrLocalTimezone, zoneErr)
	}
	namedFile, err := os.Stat(filepath.Join(root, relative))
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrLocalTimezone, err)
	}
	if !os.SameFile(target, namedFile) {
		return "", false, fmt.Errorf("%w: timezone file identity differs", ErrLocalTimezone)
	}
	return name, true, nil
}

func relativeZonePath(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}
