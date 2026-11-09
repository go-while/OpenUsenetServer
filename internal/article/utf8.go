package article

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// StorableText reports whether raw is valid UTF-8 without NUL bytes, which
// the PostgreSQL TEXT columns for headers and body require. Articles that
// fail are refused rather than converted, so stored bytes stay unchanged.
func StorableText(raw []byte) bool {
	return utf8.Valid(raw) && bytes.IndexByte(raw, 0) < 0
}

// SanitizeUTF8 makes text safe for PostgreSQL UTF-8 storage.
// NUL bytes are stripped (Postgres text rejects them). Invalid UTF-8
// sequences are treated as ISO-8859-1.
func SanitizeUTF8(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s
	}
	b := []byte(s)
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c == 0 || (c < 0x20 && c != '\t' && c != '\n' && c != '\r') {
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			out.WriteRune(r)
			i += size
			continue
		}
		out.WriteRune(rune(c))
		i++
	}
	return out.String()
}
