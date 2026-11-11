package article

import "testing"

// Heads stored by older versions can hold lines Parse now refuses (here a field
// name with a space); the web reader must still decode their body.
func TestDecodeBodyForDisplayLegacyHead(t *testing.T) {
	head := "Bad Name: legacy\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64"
	if got := DecodeBodyForDisplay(head, "aGVsbG8=\r\n"); got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

// HeaderValue unfolds a field over continuation lines, in CRLF and LF heads.
func TestHeaderValueFolded(t *testing.T) {
	for _, head := range []string{
		"Subject: s\r\nPath: peer1\r\n\t!peer2!not-for-mail\r\nFrom: a@b",
		"Subject: s\nPath: peer1\n\t!peer2!not-for-mail\nFrom: a@b",
	} {
		if got := HeaderValue(head, "path"); got != "peer1 !peer2!not-for-mail" {
			t.Errorf("HeaderValue(%q) = %q", head, got)
		}
	}
}

// A folded Path (RFC 5536 3.1.5 allows folding before "!") still names its sites.
func TestPathContainsFolded(t *testing.T) {
	path := "peer1 !peer2!not-for-mail"
	for _, site := range []string{"peer1", "peer2"} {
		if !PathContains(path, site) {
			t.Errorf("PathContains(%q, %q) = false", path, site)
		}
	}
}
