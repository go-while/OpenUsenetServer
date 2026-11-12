package nntp_test

import (
	"bufio"
	"strings"
	"testing"
)

func TestPostRefusesInjectionInfo(t *testing.T) {
	c, st := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	_, _ = c.Write([]byte("POST\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "340 ") {
		t.Fatalf("post cont %q", l)
	}
	_, _ = c.Write([]byte("From: a@b.c\r\nNewsgroups: local.test\r\nSubject: forged\r\n" +
		"Injection-Info: news.test; posting-account=\"alice\"\r\n\r\nbody\r\n.\r\n"))
	l := readLine(t, r)
	if !strings.HasPrefix(l, "441 ") || !strings.Contains(l, "Injection-Info") {
		t.Fatalf("want 441 naming Injection-Info, got %q", l)
	}
	if n, _ := st.CountArticles(t.Context()); n != 0 {
		t.Fatalf("%d articles stored", n)
	}
	// Without the header the same post goes through on the same session.
	_, _ = c.Write([]byte("POST\r\n"))
	_ = readLine(t, r)
	_, _ = c.Write([]byte("From: a@b.c\r\nNewsgroups: local.test\r\nSubject: fine\r\n\r\nbody\r\n.\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "240 ") {
		t.Fatalf("clean post %q", l)
	}
}

func TestIHaveKeepsInjectionInfo(t *testing.T) {
	c, _ := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	msgid := "<inj-info-1@origin.example>"
	_, _ = c.Write([]byte("IHAVE " + msgid + "\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "335 ") {
		t.Fatalf("ihave cont %q", l)
	}
	_, _ = c.Write([]byte("Path: origin.example!not-for-mail\r\nFrom: a@b.c\r\nNewsgroups: local.test\r\nSubject: transit\r\n" +
		"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\nMessage-ID: " + msgid + "\r\n" +
		"Injection-Info: origin.example; posting-account=\"alice\"\r\n\r\nbody\r\n.\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "235 ") {
		t.Fatalf("ihave %q", l)
	}
	_, _ = c.Write([]byte("HEAD " + msgid + "\r\n"))
	first, lines := readBlock(t, r)
	if !strings.HasPrefix(first, "221 ") {
		t.Fatalf("head %q", first)
	}
	want := `Injection-Info: origin.example; posting-account="alice"`
	if blob := strings.Join(lines, "\n"); !strings.Contains(blob, want) {
		t.Fatalf("headers lack %q:\n%s", want, blob)
	}
}
