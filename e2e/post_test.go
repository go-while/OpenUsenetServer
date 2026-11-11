//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/article"
)

// A POST without a From header is refused with 441 and nothing is stored.
// (Ported from the unit test of article.InjectForPost: here it is checked on the wire.)
func TestPostInjectRequiresFrom(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	login(c, user, pass)
	art := article.New(t, article.Options{
		Newsgroups: group,
		Subject:    "x",
		Omit:       []string{"Path", "From", "Message-ID", "Date"},
		Body:       []string{"body"},
	})
	// ExpectPost: the 441 must come after the article (340 first), not instead of the 340.
	line := c.ExpectPost(441, art.Bytes())
	if !strings.Contains(strings.ToLower(line), "from") {
		t.Fatalf("441 should name the missing From header: %q", line)
	}
	if got := c.ExpectCmd(211, "GROUP %s", group); !strings.HasPrefix(got, "211 0 ") {
		t.Fatalf("rejected post should leave the group empty: %q", got)
	}
}
