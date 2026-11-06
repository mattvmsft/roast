package review

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode"
)

// Fingerprint returns a 16-character hex digest that is a SIMILARITY key for
// a finding — "probably the same claim as a prior sighting at this location"
// — never an identity (that is Finding.ID). The digest is
// sha256 of:
//
//	file + "\x00" + normalized(code_quote)
//
// where normalized(code_quote) collapses all whitespace runs to a single
// space and trims leading/trailing whitespace.
//
// The description is intentionally excluded because models commonly reword it.
// Two different bugs quoting the same lines can therefore collide, which is
// acceptable for a similarity hint that never suppresses findings.
func Fingerprint(f Finding) string {
	normalized := collapseWhitespace(f.CodeQuote)
	key := f.File + "\x00" + normalized
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:8]) // 8 bytes → 16 hex chars
}

// collapseWhitespace replaces every run of whitespace characters with a single
// space, then trims leading and trailing spaces.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteRune(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
