//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/client"
)

// postRaw sends POST and the raw article (CRLF lines, dot-stuffed, ending in "\r\n.\r\n")
// on a logged-in c and returns the final status line, which must carry code want.
func postRaw(c *client.Conn, want int, raw string) string {
	c.ExpectCmd(340, "POST")
	c.MustSend(raw)
	return c.ExpectStatus(want, "POST")
}

// A POST without a From header is refused with 441 and nothing is stored.
// (Ported from the unit test of article.InjectForPost: here it is checked on the wire.)
func TestPostInjectRequiresFrom(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	login(c, user, pass)
	line := postRaw(c, 441, "Newsgroups: "+group+"\r\nSubject: x\r\n\r\nbody\r\n.\r\n")
	if !strings.Contains(strings.ToLower(line), "from") {
		t.Fatalf("441 should name the missing From header: %q", line)
	}
	if got := c.ExpectCmd(211, "GROUP %s", group); !strings.HasPrefix(got, "211 0 ") {
		t.Fatalf("rejected post should leave the group empty: %q", got)
	}
}
