package output

import (
	"strings"
	"unicode/utf8"
)

// Clean makes text from the server safe to write to a terminal.
//
// A build log, an error message or a name in a table is written by somebody
// else — a dependency's install script, a branch someone pushed — and a
// terminal acts on the escape sequences in what it prints: moving the
// cursor to rewrite lines already shown, clearing the screen, setting the
// window title, writing the clipboard (OSC 52), or asking the terminal to
// answer back. Printed raw, a log could make `vallic deploy` show a success
// that was not one, or put a command on the clipboard.
//
// Kept: text, newlines, tabs, carriage returns (progress bars redraw a
// line with them, which hides nothing a log did not already print), and
// colour (SGR: `ESC [ digits ; … m`), which changes how text looks and
// nothing else. Removed: every other escape sequence, every other control
// character, and the C1 controls, which some terminals read as the escape
// sequences they stand for.
func Clean(s string) string {
	if !needsCleaning(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		c := s[i]

		if c == 0x1b {
			n, keep := escape(s[i:])
			if keep {
				b.WriteString(s[i : i+n])
			}
			i += n
			continue
		}

		if c < 0x20 || c == 0x7f {
			if c == '\n' || c == '\t' || c == '\r' {
				b.WriteByte(c)
			}
			i++
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Not UTF-8: a byte a terminal might read as a C1 control.
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		if r >= 0x80 && r <= 0x9f {
			i += size
			continue
		}

		b.WriteString(s[i : i+size])
		i += size
	}

	return b.String()
}

// needsCleaning reports whether s holds anything Clean would change, so the
// common case — a plain log line — is returned as it is.
func needsCleaning(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\t' && c != '\r') || c == 0x7f || c >= 0x80 {
			return true
		}
	}
	return false
}

// escape measures the escape sequence at the start of s, and says whether
// it is colour, the one kind kept.
func escape(s string) (length int, keep bool) {
	if len(s) < 2 {
		return len(s), false
	}

	switch s[1] {
	case '[':
		// CSI: parameters and intermediates, then one final byte.
		for i := 2; i < len(s); i++ {
			c := s[i]
			if c >= 0x40 && c <= 0x7e {
				return i + 1, c == 'm' && colourParameters(s[2:i])
			}
			if c < 0x20 || c > 0x3f {
				// Not a well-formed sequence: drop the ESC and what came so far.
				return i, false
			}
		}
		return len(s), false

	case ']', 'P', 'X', '^', '_':
		// OSC, DCS, SOS, PM, APC: a string ended by BEL or ESC \.
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1, false
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2, false
			}
		}
		return len(s), false

	default:
		// A two-byte sequence: ESC and one more.
		return 2, false
	}
}

// colourParameters reports whether a CSI's parameters are an SGR's: digits
// and semicolons, nothing that turns it into another command.
func colourParameters(p string) bool {
	for i := 0; i < len(p); i++ {
		if (p[i] < '0' || p[i] > '9') && p[i] != ';' {
			return false
		}
	}
	return true
}
