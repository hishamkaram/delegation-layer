// Package terminal owns the presentation boundary for untrusted task text.
package terminal

import (
	"strings"
	"unicode"
)

// Text never interprets provider-supplied terminal controls. Sequence state is
// carried between chunks so a split OSC/CSI cannot escape the display boundary.
type (
	Text          struct{ state sequenceState }
	sequenceState uint8
)

const (
	plain sequenceState = iota
	escape
	csi
	controlString
	stringEscape
)

func (t *Text) Write(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch t.state {
		case plain:
			t.plainRune(&out, r)
		case escape:
			t.escapeRune(r)
		case csi:
			if r >= 0x40 && r <= 0x7e {
				t.state = plain
			}
		case controlString:
			switch r {
			case 0x07, 0x9c:
				t.state = plain
			case 0x1b:
				t.state = stringEscape
			}
		case stringEscape:
			switch r {
			case '\\', 0x9c:
				t.state = plain
			case 0x1b:
			default:
				t.state = controlString
			}

		}
	}
	return out.String()
}

func (t *Text) plainRune(out *strings.Builder, r rune) {
	switch r {
	case 0x1b:
		t.state = escape
	case 0x9b:
		t.state = csi
	case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
		t.state = controlString
	case '\n', '\t':
		out.WriteRune(r)
	default:
		if !unicode.IsControl(r) && !directionalControl(r) {
			out.WriteRune(r)
		}
	}
}

func Safe(s string) string { var t Text; return t.Write(s) }

func directionalControl(r rune) bool { return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 }

func (t *Text) escapeRune(r rune) {
	switch r {
	case '[':
		t.state = csi
	case ']', 'P', '^', '_', 'X':
		t.state = controlString
	default:
		if r >= 0x30 && r <= 0x7e {
			t.state = plain
		}
	}
}
