package nntp_test

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

// xrefLines returns the Xref field lines of a served head.
func xrefLines(head string) []string {
	var out []string
	for _, l := range strings.Split(head, "\r\n") {
		if name, _, ok := strings.Cut(l, ":"); ok && strings.EqualFold(name, "Xref") {
			out = append(out, l)
		}
	}
	return out
}

// ihave sends one article by IHAVE and expects 235.
func ihave(t *testing.T, c net.Conn, r *bufio.Reader, msgid, head, body string) {
	t.Helper()
	_, _ = c.Write([]byte("IHAVE " + msgid + "\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "335 ") {
		t.Fatalf("ihave cont %q", l)
	}
	stuffed := strings.ReplaceAll(body, "\r\n.", "\r\n..")
	_, _ = c.Write([]byte(head + "\r\n" + stuffed + ".\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "235 ") {
		t.Fatalf("ihave %q", l)
	}
}

// fetchRaw sends cmd and returns the status line and the raw block.
func fetchRaw(t *testing.T, c net.Conn, r *bufio.Reader, cmd, wantCode string) string {
	t.Helper()
	_, _ = c.Write([]byte(cmd + "\r\n"))
	first, got := readRawBlock(t, r)
	if !strings.HasPrefix(first, wantCode+" ") {
		t.Fatalf("%s: %q, want %s", cmd, first, wantCode)
	}
	return got
}

// TestServedArticleCarriesXref covers O-10: ARTICLE and HEAD (by number and
// by message-id) add the stored Xref value as the last header line, the rest
// of the head stays as stored, BODY is unchanged.
func TestServedArticleCarriesXref(t *testing.T) {
	c, _ := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)

	msgid := "<xref-1@news.test>"
	head := "X-Zeta: unknown header first\r\n" +
		"Path: other!not-for-mail\r\n" +
		"Subject: a folded\r\n" +
		"\tsubject line\r\n" +
		"From: a@b.c\r\n" +
		"X-Folded: first\r\n" +
		"   second\r\n" +
		"Newsgroups: local.test\r\n" +
		"x-lower:no-space\r\n" +
		"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
		"Message-ID: " + msgid + "\r\n" +
		"X-Trace: one\r\n" +
		"X-Trace: two\r\n"
	body := "col1\rcol2\r\n" +
		".dot line\r\n" +
		"\r\n" +
		"last\r\n"
	ihave(t, c, r, msgid, head, body)

	wantHead := strings.Replace(head, "Path: other!", "Path: news.test!other!", 1) +
		"Xref: news.test local.test:1\r\n"

	_, _ = c.Write([]byte("GROUP local.test\r\n"))
	_ = readLine(t, r)
	for _, sel := range []string{msgid, "1"} {
		got := fetchRaw(t, c, r, "ARTICLE "+sel, "220")
		if want := wantHead + "\r\n" + body; got != want {
			t.Errorf("ARTICLE %s\n%q\nwant\n%q", sel, got, want)
		}
		got = fetchRaw(t, c, r, "HEAD "+sel, "221")
		if got != wantHead {
			t.Errorf("HEAD %s\n%q\nwant\n%q", sel, got, wantHead)
		}
		if x := xrefLines(got); len(x) != 1 {
			t.Errorf("HEAD %s: Xref lines %q, want one", sel, x)
		}
		if got = fetchRaw(t, c, r, "BODY "+sel, "222"); got != body {
			t.Errorf("BODY %s\n%q\nwant\n%q", sel, got, body)
		}
	}

	// HDR and OVER show the same value.
	if got := fetchRaw(t, c, r, "HDR Xref 1", "225"); got != "1 news.test local.test:1\r\n" {
		t.Errorf("HDR Xref by number %q", got)
	}
	if got := fetchRaw(t, c, r, "HDR xref "+msgid, "225"); got != "0 news.test local.test:1\r\n" {
		t.Errorf("HDR Xref by message-id %q", got)
	}
	for _, sel := range []string{"1", msgid} {
		got := fetchRaw(t, c, r, "OVER "+sel, "224")
		if !strings.HasSuffix(got, "\tXref: news.test local.test:1\r\n") {
			t.Errorf("OVER %s %q", sel, got)
		}
	}
}

// TestServedXrefReplacesPeerXref: an Xref received from a peer is never
// served as ours. IHAVE drops it at intake; a stored head that still has one
// (an older store or an import) gets it replaced in place when served, with
// its continuation lines and any second Xref field removed.
func TestServedXrefReplacesPeerXref(t *testing.T) {
	c, st := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)

	msgid := "<xref-peer-ihave@news.test>"
	head := "Path: peer!not-for-mail\r\n" +
		"From: a@b.c\r\n" +
		"Newsgroups: local.test\r\n" +
		"Subject: peer xref\r\n" +
		"Xref: peer.example local.test:77\r\n" +
		"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
		"Message-ID: " + msgid + "\r\n"
	ihave(t, c, r, msgid, head, "body\r\n")
	got := fetchRaw(t, c, r, "HEAD "+msgid, "221")
	if x := xrefLines(got); len(x) != 1 || x[0] != "Xref: news.test local.test:1" {
		t.Fatalf("IHAVE'd peer Xref served as %q", x)
	}

	msgid = "<xref-peer-stored@news.test>"
	stored := "Path: news.test!peer!not-for-mail\r\n" +
		"From: a@b.c\r\n" +
		"Newsgroups: local.test\r\n" +
		"XREF:   peer.example local.test:77\r\n" +
		"\tlocal.other:3\r\n" +
		"Subject: stored peer xref\r\n" +
		"Xref: peer.example local.test:78\r\n" +
		"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
		"Message-ID: " + msgid
	if _, err := st.Post(context.Background(), stored, "body\r\n", msgid, "stored peer xref",
		"a@b.c", "Mon, 01 Jan 2024 00:00:00 +0000", "", "news.test", 0, 1,
		[]string{"local.test"}, false); err != nil {
		t.Fatal(err)
	}
	want := "Path: news.test!peer!not-for-mail\r\n" +
		"From: a@b.c\r\n" +
		"Newsgroups: local.test\r\n" +
		"XREF: news.test local.test:2\r\n" +
		"Subject: stored peer xref\r\n" +
		"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
		"Message-ID: " + msgid + "\r\n"
	if got = fetchRaw(t, c, r, "HEAD "+msgid, "221"); got != want {
		t.Errorf("HEAD\n%q\nwant\n%q", got, want)
	}
	if got = fetchRaw(t, c, r, "ARTICLE "+msgid, "220"); got != want+"\r\nbody\r\n" {
		t.Errorf("ARTICLE\n%q\nwant\n%q", got, want+"\r\nbody\r\n")
	}
	// The stored head itself is not changed by serving.
	a, err := st.GetByMsgID(context.Background(), msgid)
	if err != nil || a == nil || a.Headers != stored {
		t.Errorf("stored head changed: %v %+v", err, a)
	}
}

// TestOverBytesCountServedXref: :bytes in OVER and HDR is the size of the
// article as ARTICLE serves it (RFC 3977 8.5.2), so it counts our Xref line,
// also when it replaces an Xref field kept in the stored head (an import).
func TestOverBytesCountServedXref(t *testing.T) {
	c, st := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)

	ihave(t, c, r, "<xref-bytes-1@news.test>", "Path: p!x\r\nFrom: a@b.c\r\nNewsgroups: local.test\r\n"+
		"Subject: s\r\nDate: Mon, 01 Jan 2024 00:00:00 +0000\r\nMessage-ID: <xref-bytes-1@news.test>\r\n", "b\r\n")
	head := "Path: p!x\r\nFrom: a@b.c\r\nNewsgroups: local.test\r\nXref: an.old.server.example local.test:12345\r\n" +
		"\tlocal.other:1\r\nSubject: s\r\nDate: Mon, 01 Jan 2024 00:00:00 +0000\r\nMessage-ID: <xref-bytes-2@news.test>"
	body := "b\r\n"
	if _, err := st.Post(context.Background(), head, body, "<xref-bytes-2@news.test>", "s", "a@b.c",
		"Mon, 01 Jan 2024 00:00:00 +0000", "", "news.test", len(head)+4+len(body), 1,
		[]string{"local.test"}, false); err != nil {
		t.Fatal(err)
	}

	_, _ = c.Write([]byte("GROUP local.test\r\n"))
	_ = readLine(t, r)
	for _, n := range []string{"1", "2"} {
		size := strconv.Itoa(len(fetchRaw(t, c, r, "ARTICLE "+n, "220")))
		f := strings.Split(strings.TrimSuffix(fetchRaw(t, c, r, "OVER "+n, "224"), "\r\n"), "\t")
		if len(f) != 9 || f[6] != size {
			t.Errorf("OVER %s: :bytes %q, want %s (ARTICLE size)", n, f[6], size)
		}
		if got := fetchRaw(t, c, r, "HDR :bytes "+n, "225"); got != n+" "+size+"\r\n" {
			t.Errorf("HDR :bytes %s: %q, want %s", n, got, size)
		}
	}
}

// TestServedXrefCrosspost: a cross-posted article lists every group.
func TestServedXrefCrosspost(t *testing.T) {
	c, st := startTestServer(t)
	if err := st.EnsureGroup(context.Background(), "local.other", "Other", "y"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	_ = readLine(t, r)

	// One article in local.other first, so the numbers differ.
	ihave(t, c, r, "<xref-x0@news.test>", "Path: p!x\r\nFrom: a@b.c\r\nNewsgroups: local.other\r\n"+
		"Subject: s\r\nDate: Mon, 01 Jan 2024 00:00:00 +0000\r\nMessage-ID: <xref-x0@news.test>\r\n", "b\r\n")
	msgid := "<xref-x1@news.test>"
	ihave(t, c, r, msgid, "Path: p!x\r\nFrom: a@b.c\r\nNewsgroups: local.test,local.other\r\n"+
		"Subject: s\r\nDate: Mon, 01 Jan 2024 00:00:00 +0000\r\nMessage-ID: "+msgid+"\r\n", "b\r\n")

	want := "Xref: news.test local.test:1 local.other:2"
	got := fetchRaw(t, c, r, "HEAD "+msgid, "221")
	if x := xrefLines(got); len(x) != 1 || x[0] != want {
		t.Errorf("HEAD %s: Xref %q, want %q", msgid, x, want)
	}
	_, _ = c.Write([]byte("GROUP local.other\r\n"))
	_ = readLine(t, r)
	got = fetchRaw(t, c, r, "ARTICLE 2", "220")
	if x := xrefLines(strings.SplitN(got, "\r\n\r\n", 2)[0]); len(x) != 1 || x[0] != want {
		t.Errorf("ARTICLE 2 in local.other: Xref %q, want %q", x, want)
	}
}
