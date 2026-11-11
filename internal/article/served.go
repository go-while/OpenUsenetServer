package article

import "strings"

// ServedHead returns a stored head (lines joined by CRLF, or by LF in older
// stored heads, without the empty separator line) with its Xref field set to
// xref, for ARTICLE and HEAD replies.
//
// Xref is local to the serving agent (RFC 5536 §3.2.14, RFC 5537 §3.3): it
// is not part of the stored article and is not sent to peers, but readers
// expect it in what we serve, and it must agree with the Xref:full overview
// field. The first Xref field (any case), with its continuation lines, is
// replaced in place by "<name>: <xref>"; further Xref fields are removed; with
// none, the line "Xref: <xref>" is appended. Every other byte of the head is
// kept as stored. An empty xref returns head unchanged.
func ServedHead(head, xref string) string {
	if xref == "" {
		return head
	}
	if head == "" {
		return "Xref: " + xref
	}
	eol := "\r\n"
	if !strings.Contains(head, "\r\n") && strings.Contains(head, "\n") {
		eol = "\n"
	}
	var b strings.Builder
	b.Grow(len(head) + len(eol) + len("Xref: ") + len(xref))
	replaced, skipping := false, false
	// Each piece keeps its line terminator; the last one has none.
	for _, piece := range strings.SplitAfter(head, "\n") {
		line := strings.TrimSuffix(strings.TrimSuffix(piece, "\n"), "\r")
		if skipping && isContinuation(line) {
			continue
		}
		colon, isXref := fieldColon(line, "Xref")
		skipping = isXref
		switch {
		case !isXref:
			b.WriteString(piece)
		case !replaced:
			replaced = true
			b.WriteString(line[:colon] + ": " + xref + piece[len(line):])
		}
	}
	out := b.String()
	if !strings.HasSuffix(head, "\n") {
		// A dropped Xref field at the end leaves the line terminator of
		// the field before it.
		out = strings.TrimSuffix(strings.TrimSuffix(out, "\n"), "\r")
	}
	if !replaced {
		if !strings.HasSuffix(out, "\n") {
			out += eol
		}
		out += "Xref: " + xref
	}
	return out
}
