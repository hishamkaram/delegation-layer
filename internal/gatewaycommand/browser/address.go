package browser

import (
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// socketAddress accepts only one selected local bus, never bus autolaunch,
// transport fallback or environment mutation.
func socketAddress(environment []string) (string, error) {
	raw := verifiedexec.EnvironmentValue(environment, "DBUS_SESSION_BUS_ADDRESS")
	if len(raw) > 4096 || !strings.HasPrefix(raw, "unix:") || strings.Contains(raw, ";") {
		return "", errActivation
	}
	values := make(map[string]string, 3)
	for field := range strings.SplitSeq(strings.TrimPrefix(raw, "unix:"), ",") {
		key, encoded, ok := strings.Cut(field, "=")
		if !ok || (key != "path" && key != "abstract" && key != "guid") {
			return "", errActivation
		}
		if _, duplicate := values[key]; duplicate {
			return "", errActivation
		}
		value, err := dbus.UnescapeBusAddressValue(encoded)
		if err != nil || value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return "", errActivation
		}
		values[key] = value
	}
	return selectedSocket(values)
}

func selectedSocket(values map[string]string) (string, error) {
	if guid, supplied := values["guid"]; supplied {
		if _, err := hex.DecodeString(guid); err != nil || len(guid) != 32 {
			return "", errActivation
		}
	}
	path, file := values["path"]
	abstract, named := values["abstract"]
	if file == named {
		return "", errActivation
	}
	if file {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", errActivation
		}
		return path, nil
	}
	if len(abstract) > 107 {
		return "", errActivation
	}
	return "@" + abstract, nil
}
