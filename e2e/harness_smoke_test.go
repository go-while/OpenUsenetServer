//go:build e2e

package e2e

import (
	"testing"

	"github.com/go-while/nntp-testkit/client"
)

// The harness itself: greeting, login, a posted article reaches the group.
func TestHarnessPostAndRead(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	login(c, user, pass)
	c.ExpectCmd(340, "POST")
	art := testArticle("<harness-1@e2e.test>", group)
	c.MustSend(client.Stuff(art))
	c.ExpectStatus(240, "POST")
	c.ExpectCmd(211, "GROUP %s", group)
	c.ExpectCmd(220, "ARTICLE <harness-1@e2e.test>")
	if got := c.ReadDotLines(); len(got) == 0 {
		t.Fatal("empty article")
	}
}
