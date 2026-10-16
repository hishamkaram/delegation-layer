package lifetime

import "strings"

// BootRelation describes only what stable kernel boot identities prove.
// Unknown includes older timestamp-based identities and different OS schemes.
type BootRelation uint8

const (
	BootUnknown BootRelation = iota
	BootSame
	BootDifferent
)

// CompareBootIDs never treats a clock-based identity or an unknown scheme as
// evidence of a reboot. Persisted records remain unchanged.
func CompareBootIDs(recorded, current string) BootRelation {
	recordedScheme, recordedUUID := stableBootIdentity(recorded)
	currentScheme, currentUUID := stableBootIdentity(current)
	if recordedScheme == "" || recordedScheme != currentScheme {
		return BootUnknown
	}
	if recordedUUID == currentUUID {
		return BootSame
	}
	return BootDifferent
}

func stableBootIdentity(value string) (string, string) {
	for _, prefix := range []string{"linux:", "darwin:boot-session:"} {
		if encoded, ok := strings.CutPrefix(value, prefix); ok {
			if uuid, valid := canonicalBootUUID(encoded); valid {
				return prefix, uuid
			}
			break
		}
	}
	return "", ""
}

func canonicalBootUUID(value string) (string, bool) {
	if len(value) != 36 {
		return "", false
	}
	for i := range len(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return "", false
			}
			continue
		}
		c := value[i]
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(c)) {
			return "", false
		}
	}
	return strings.ToLower(value), true
}
