// Package textsafe neutralizes control characters in model- and
// repository-derived text before Roast prints it to a terminal or writes it
// into a human-readable report.
//
// Finding text comes from a model that read an untrusted repository, so a JSON
// string such as "\u001b]52;c;..." decodes to a real ESC byte. Printed raw,
// that byte would let reviewed content retitle the terminal, rewrite the
// clipboard, or repaint the screen. JSON output does not need this package:
// encoding/json already escapes every control character.
package textsafe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Text renders C0 controls other than newline and tab, DEL, C1 controls
// (U+0080-U+009F), and bytes that are not valid UTF-8 as visible escapes. A
// CRLF pair becomes a plain newline so CRLF source files still read normally.
// Text is idempotent: its output contains nothing it would escape again.
func Text(s string) string {
	return escape(s, false)
}

// Line is Text for single-line output: newline and tab are escaped too, so the
// value cannot start a new line of its own.
func Line(s string) string {
	return escape(s, true)
}

func escape(s string, oneLine bool) string {
	if clean(s, oneLine) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\r' && i+1 < len(s) && s[i+1] == '\n' && !oneLine:
			// Dropped; the following '\n' is kept by the default case.
		case r == '\n' && oneLine:
			b.WriteString(`\n`)
		case r == '\t' && oneLine:
			b.WriteString(`\t`)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// clean reports whether s needs no escaping, so the common case allocates
// nothing.
func clean(s string, oneLine bool) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c == 0x7f || (c < 0x20 && (oneLine || (c != '\n' && c != '\t'))) {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || (r >= 0x80 && r <= 0x9f) {
			return false
		}
		i += size
	}
	return true
}
