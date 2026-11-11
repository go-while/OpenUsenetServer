//go:build e2e

package e2e

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// Reader tests ported from internal/nntp/session_test.go. The unit tests drive a bare Session
// over net.Pipe with no users in the store (open posting); here the full server runs with the
// harness user, so a test logs in before it posts.

// The greeting is 200, CAPABILITIES lists the reader, posting, transit and auth capabilities,
// and QUIT answers 205 and closes the connection.
func TestGreetingAndCapabilities(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	if !strings.HasPrefix(c.Greeting, "200 ") {
		t.Fatalf("greeting %q", c.Greeting)
	}
	caps, lines := c.Capabilities()
	if v := caps["VERSION"]; len(v) == 0 || v[0] != "2" {
		t.Fatalf("VERSION 2 missing: %q", lines)
	}
	for _, want := range []string{"READER", "POST", "LIST", "STREAMING", "IHAVE"} {
		if _, ok := caps[want]; !ok {
			t.Fatalf("capabilities missing %s: %q", want, lines)
		}
	}
	hasArg := func(label, arg string) bool {
		for _, a := range caps[label] {
			if strings.EqualFold(a, arg) {
				return true
			}
		}
		return false
	}
	if !hasArg("OVER", "MSGID") {
		t.Fatalf("should advertise OVER MSGID: %q", lines)
	}
	if !hasArg("AUTHINFO", "USER") {
		t.Fatalf("should advertise AUTHINFO USER: %q", lines)
	}
	if len(caps["LIST"]) == 0 {
		t.Fatalf("LIST should name its keywords: %q", lines)
	}
	c.ExpectCmd(205, "QUIT")
	if _, err := c.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("after 205 the server should close: %v", err)
	}
}

// An unknown command answers 500 and a known command with a bad argument answers 501.
func TestUnknownAndSyntax(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	c.ExpectCmd(500, "MAIL")
	c.ExpectCmd(501, "MODE POSTER")
}

// LIST ACTIVE shows the group, GROUP of a missing group is 411, a POST is stored with an
// injected Message-ID and is readable by ARTICLE, STAT and OVER (with the Xref field), and a
// pipelined STAT plus ARTICLE of an unknown id get 223 and 430 in order.
func TestGroupListPostArticle(t *testing.T) {
	e := startEnv(t)
	c := e.dial()

	c.ExpectCmd(215, "LIST ACTIVE")
	found := false
	for _, l := range c.ReadDotLines() {
		if strings.HasPrefix(l, group+" ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("LIST ACTIVE does not list %s", group)
	}

	c.ExpectCmd(411, "GROUP missing.group")

	login(c, user, pass)
	c.ExpectPost(240, testPost(t, "", "hello", "body line").Bytes()) // no Message-ID: injected

	if g := strings.Fields(c.ExpectCmd(211, "GROUP %s", group)); len(g) < 5 || g[1] != "1" || g[4] != group {
		t.Fatalf("GROUP should report 1 article in %s: %q", group, g)
	}

	c.ExpectCmd(220, "ARTICLE 1")
	blob := strings.Join(c.ReadDotLines(), "\n")
	if !strings.Contains(blob, "Subject: hello") || !strings.Contains(blob, "body line") {
		t.Fatalf("article %q", blob)
	}
	if !strings.Contains(blob, "Message-ID:") {
		t.Fatalf("missing injected Message-ID: %q", blob)
	}

	c.MustSend("STAT 1\r\nARTICLE <no-such@id>\r\n")
	c.ExpectStatus(223, "STAT 1")
	c.ExpectStatus(430, "ARTICLE <no-such@id>")

	c.ExpectCmd(224, "OVER 1-1")
	over := c.ReadDotLines()
	if len(over) != 1 || !strings.Contains(over[0], "hello") {
		t.Fatalf("overview %q", over)
	}
	if strings.Contains(over[0], "Xref:full") || !strings.Contains(over[0], "\tXref: "+pathhost+" "+group+":1") {
		t.Fatalf("xref overview field: %q", over[0])
	}
}

// ARTICLE <msgid> answers "220 0 <msgid>" and leaves the current article number unchanged.
func TestFetchByMsgidDoesNotChangeCurrent(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	login(c, user, pass)
	c.ExpectPost(240, testPost(t, "", "one", "x").Bytes())
	c.ExpectPost(240, testPost(t, "", "two", "y").Bytes())

	c.ExpectCmd(211, "GROUP %s", group)
	msgid := func(n int) string {
		line := c.ExpectCmd(223, "STAT %d", n)
		f := strings.Fields(line)
		if len(f) < 3 || !strings.HasPrefix(f[2], "<") {
			t.Fatalf("STAT %d: %q", n, line)
		}
		return f[2]
	}
	msgid1 := msgid(1)
	msgid2 := msgid(2)

	if first := c.ExpectCmd(220, "ARTICLE %s", msgid1); !strings.HasPrefix(first, "220 0 "+msgid1) {
		t.Fatalf("msgid fetch should report number 0: %q", first)
	}
	c.ReadDotLines()
	if cur := c.ExpectCmd(223, "STAT"); !strings.HasPrefix(cur, "223 2 "+msgid2) {
		t.Fatalf("current should still be article 2 (%s), got %q", msgid2, cur)
	}
}

// Pipelined GROUP and STAT are answered in order: 211, then 223 for the first article.
func TestPipelinedGroupStat(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	login(c, user, pass)
	c.ExpectPost(240, testPost(t, "", "p", "z").Bytes())
	c.MustSend("GROUP " + group + "\r\nSTAT\r\n")
	c.ExpectStatus(211, "GROUP")
	c.ExpectStatus(223, "STAT")
	// The next reply must be DATE's: no stray line was queued after the two answers.
	c.ExpectCmd(111, "DATE")
}
