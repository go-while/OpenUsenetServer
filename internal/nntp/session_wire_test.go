package nntp_test

import (
	"bufio"
	"strings"
	"testing"
)

// readRawBlock reads a status line and a multi-line block and returns the
// block un-dot-stuffed with its CRLF line ends, so it can be compared byte
// for byte.
func readRawBlock(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	first := readLine(t, r)
	var b strings.Builder
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == ".\r\n" {
			break
		}
		b.WriteString(strings.TrimPrefix(l, "."))
	}
	return first, b.String()
}

func TestIHaveKeepsArticleBytes(t *testing.T) {
	c, _ := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	msgid := "<bytes-1@news.test>"
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
	_, _ = c.Write([]byte("IHAVE " + msgid + "\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "335 ") {
		t.Fatalf("ihave cont %q", l)
	}
	stuffed := strings.ReplaceAll(body, "\r\n.", "\r\n..")
	_, _ = c.Write([]byte(head + "\r\n" + stuffed + ".\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "235 ") {
		t.Fatalf("ihave %q", l)
	}

	wantHead := strings.Replace(head, "Path: other!", "Path: news.test!other!", 1)
	_, _ = c.Write([]byte("ARTICLE " + msgid + "\r\n"))
	first, got := readRawBlock(t, r)
	if !strings.HasPrefix(first, "220 ") {
		t.Fatalf("article %q", first)
	}
	if want := wantHead + "\r\n" + body; got != want {
		t.Fatalf("ARTICLE\n%q\nwant\n%q", got, want)
	}
	_, _ = c.Write([]byte("HEAD " + msgid + "\r\n"))
	if first, got = readRawBlock(t, r); !strings.HasPrefix(first, "221 ") || got != wantHead {
		t.Fatalf("HEAD %q\n%q\nwant\n%q", first, got, wantHead)
	}
	_, _ = c.Write([]byte("BODY " + msgid + "\r\n"))
	if first, got = readRawBlock(t, r); !strings.HasPrefix(first, "222 ") || got != body {
		t.Fatalf("BODY %q\n%q\nwant\n%q", first, got, body)
	}

	_, _ = c.Write([]byte("HDR X-Folded " + msgid + "\r\n"))
	if first, got = readRawBlock(t, r); !strings.HasPrefix(first, "225 ") || got != "0 first second\r\n" {
		t.Fatalf("HDR by message-id %q %q", first, got)
	}
	_, _ = c.Write([]byte("GROUP local.test\r\n"))
	_ = readLine(t, r)
	_, _ = c.Write([]byte("HDR x-folded 1\r\n"))
	if first, got = readRawBlock(t, r); !strings.HasPrefix(first, "225 ") || got != "1 first second\r\n" {
		t.Fatalf("HDR by number %q %q", first, got)
	}
}

// badHead has a line without a colon (an mbox envelope line).
const badHead = "From nobody Thu Dec  8 12:55:04 2016\r\n" +
	"Path: other!not-for-mail\r\n" +
	"From: a@b.c\r\n" +
	"Newsgroups: local.test\r\n" +
	"Subject: bad head\r\n" +
	"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n"

func TestInvalidHeaderBlockReplies(t *testing.T) {
	c, _ := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)

	msgid := "<badhead-ihave@news.test>"
	_, _ = c.Write([]byte("IHAVE " + msgid + "\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "335 ") {
		t.Fatalf("ihave cont %q", l)
	}
	_, _ = c.Write([]byte(badHead + "Message-ID: " + msgid + "\r\n\r\nbody\r\n.\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "437 invalid header block") {
		t.Fatalf("IHAVE: %q, want 437 invalid header block", l)
	}

	_, _ = c.Write([]byte("MODE STREAM\r\n"))
	_ = readLine(t, r)
	msgid = "<badhead-takethis@news.test>"
	_, _ = c.Write([]byte("TAKETHIS " + msgid + "\r\n" + badHead + "Message-ID: " + msgid + "\r\n\r\nbody\r\n.\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "439 "+msgid+" invalid header block") {
		t.Fatalf("TAKETHIS: %q, want 439 invalid header block", l)
	}

	_, _ = c.Write([]byte("POST\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "340 ") {
		t.Fatalf("post cont %q", l)
	}
	_, _ = c.Write([]byte(badHead + "\r\nbody\r\n.\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "441 invalid header block") {
		t.Fatalf("POST: %q, want 441 invalid header block", l)
	}
}
