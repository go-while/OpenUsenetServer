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

// ServedSize is the size in octets of the article ARTICLE sends for a stored
// head and body with Xref value xref: WireLen of ServedHead(head, xref), the
// empty line and body, as Session.fetch assembles it. The stores keep it as
// the article's bytes, so that :bytes in OVER and HDR is the size of the
// article as served (RFC 3977 section 8.5.2).
func ServedSize(head, body, xref string) int {
	return WireLen(ServedHead(head, xref) + "\r\n\r\n" + body)
}

// WireLen is the number of octets nntp.Conn.WriteDot writes for payload,
// without the dot-stuffing and without the terminating ".\r\n". It treats
// line ends exactly as the writer does: payload is split at each LF, one CR
// right before the LF (or at the very end of payload) is dropped, and every
// line goes out followed by CRLF; nothing follows a final LF. So an LF-only
// line end counts as CRLF, and a bare CR inside a line counts as one octet.
func WireLen(payload string) int {
	n, start := 0, 0
	for start < len(payload) {
		end, next := len(payload), len(payload)
		if i := strings.IndexByte(payload[start:], '\n'); i >= 0 {
			end = start + i
			next = end + 1
		}
		line := end - start
		if line > 0 && payload[end-1] == '\r' {
			line--
		}
		n += line + 2
		start = next
	}
	return n
}
