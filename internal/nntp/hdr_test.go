package nntp_test

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

// HDR without a range is about the current article (RFC 3977 8.5.2): in a group without one
// it must answer 420, as OVER does, not 423 for an empty range.
func TestHdrWithoutCurrentArticle(t *testing.T) {
	c, _ := startTestServer(t)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	send := func(cmd, want string) string {
		t.Helper()
		if _, err := c.Write([]byte(cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		got := readLine(t, r)
		if !strings.HasPrefix(got, want) {
			t.Fatalf("%s: got %q, want %s...", cmd, got, want)
		}
		return got
	}
	send("GROUP local.test", "211 0 ")
	send("HDR Subject", "420 ")
	send("POST", "340 ")
	send("From: a@b.c\r\nNewsgroups: local.test\r\nSubject: hdr\r\n\r\nz\r\n.", "240 ")
	send("GROUP local.test", "211 1 ")
	send("HDR Subject", "225 ")
	var lines []string
	for l := readLine(t, r); l != "."; l = readLine(t, r) {
		lines = append(lines, l)
	}
	if len(lines) != 1 || lines[0] != "1 hdr" {
		t.Fatalf("HDR Subject of the current article: %q", lines)
	}
}

// A current article that expired or was cancelled after GROUP is an invalid current article:
// HDR and OVER without a range answer 420; with an explicit range the number is just missing
// (423). Found by ous-sol's review.
func TestHdrOverExpiredCurrentArticle(t *testing.T) {
	c, st := startTestServer(t)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	ctx := context.Background()
	const msgid = "<hdr-expired@test>"
	if _, err := st.Post(ctx, "Subject: kept", "body\r\n", msgid, "kept", "a@b", "now", "", "news.test", 30, 1, []string{"local.test"}, false); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	send := func(cmd, want string) {
		t.Helper()
		if _, err := c.Write([]byte(cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		if got := readLine(t, r); !strings.HasPrefix(got, want) {
			t.Fatalf("%s: got %q, want %s...", cmd, got, want)
		}
	}
	send("GROUP local.test", "211 1 ")
	if removed, err := st.CancelMessageID(ctx, msgid); err != nil || !removed {
		t.Fatalf("cancel the current article: %v, %v", removed, err)
	}
	send("HDR Subject", "420 ")
	send("HDR Subject 1", "423 ")
	send("OVER", "420 ")
	send("OVER 1", "423 ")
}

// GROUP of an empty group leaves no current article. Articles that arrive afterwards (another
// session posts) do not make one: HDR and OVER without a range must still answer 420, not list
// the whole group (the store reads 0 as "unbounded").
func TestHdrOverNoCurrentArticleAfterArrival(t *testing.T) {
	c, st := startTestServer(t)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	send := func(cmd, want string) {
		t.Helper()
		if _, err := c.Write([]byte(cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		if got := readLine(t, r); !strings.HasPrefix(got, want) {
			t.Fatalf("%s: got %q, want %s...", cmd, got, want)
		}
	}
	send("GROUP local.test", "211 0 ")
	if _, err := st.Post(context.Background(), "Subject: late", "body\r\n", "<hdr-late@test>", "late", "a@b", "now", "", "news.test", 30, 1, []string{"local.test"}, false); err != nil {
		t.Fatal(err)
	}
	send("HDR Subject", "420 ")
	send("OVER", "420 ")
}
