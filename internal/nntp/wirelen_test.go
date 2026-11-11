package nntp_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"openusenet/internal/article"
	"openusenet/internal/nntp"
)

// writeDotLen returns the number of octets Conn.WriteDot sends for payload,
// without the dot-stuffing and the terminating ".\r\n".
func writeDotLen(t *testing.T, payload string) int {
	t.Helper()
	a, b := net.Pipe()
	got := make(chan []byte, 1)
	go func() {
		out, _ := io.ReadAll(a)
		got <- out
	}()
	c := nntp.NewConn(b, 5*time.Second)
	if err := c.WriteDot([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	out := <-got
	if !bytes.HasSuffix(out, []byte(".\r\n")) {
		t.Fatalf("no terminator: %q", out)
	}
	out = out[:len(out)-3]
	stuffed := 0
	for _, line := range bytes.SplitAfter(out, []byte("\r\n")) {
		if len(line) > 0 && line[0] == '.' {
			stuffed++ // every line starting with a dot got one more
		}
	}
	return len(out) - stuffed
}

// TestServedSizeMatchesWriteDot: article.ServedSize (the stored bytes) is
// what ARTICLE writes for the head with our Xref, the empty line and the
// body, for every line-end form keep-article-bytes can store.
func TestServedSizeMatchesWriteDot(t *testing.T) {
	const x = "news.test local.test:1 local.other:2"
	heads := map[string]string{
		"crlf":      "Path: a!b\r\nSubject: s\r\nMessage-ID: <m@x>",
		"lf":        "Path: a!b\nSubject: s\nMessage-ID: <m@x>",
		"mixed":     "Path: a!b\nSubject: s\r\nMessage-ID: <m@x>",
		"folded":    "Path: a!b\r\nSubject: a\r\n\tfolded\r\nX-F: one\r\n  two",
		"xref-fold": "Path: a!b\r\nXref: peer.example a:1\r\n\tb:2\r\nSubject: s\r\nXREF: again\r\n c:3",
		"xref-lf":   "Path: a!b\nxref: peer a:1\n b:2\nSubject: s",
		"bare-cr":   "Path: a!b\r\nX-Odd: one\rtwo\r\nSubject: s",
		"empty":     "",
	}
	bodies := map[string]string{
		"crlf":          "one\r\n.two\r\n\r\nthree\r\n",
		"lf":            "one\n.two\n\nthree\n",
		"mixed":         "one\r\ntwo\nthree\r\n",
		"bare-cr":       "col1\rcol2\r\nend\r\n",
		"cr-before-lf":  "a\r\r\nb\r\n",
		"no-final-eol":  "last line",
		"final-bare-cr": "last line\r",
		"dots":          ".\r\n..\r\n.x\n",
		"empty":         "",
		"only-eol":      "\r\n",
	}
	for hn, head := range heads {
		for bn, body := range bodies {
			for _, xref := range []string{x, ""} {
				payload := article.ServedHead(head, xref) + "\r\n\r\n" + body
				want := writeDotLen(t, payload)
				if got := article.ServedSize(head, body, xref); got != want {
					t.Errorf("head %s, body %s, xref %q: ServedSize %d, WriteDot wrote %d", hn, bn, xref, got, want)
				}
				if got := article.WireLen(payload); got != want {
					t.Errorf("head %s, body %s, xref %q: WireLen %d, WriteDot wrote %d", hn, bn, xref, got, want)
				}
			}
		}
	}
}
