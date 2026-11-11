//go:build e2e

package e2e

import "testing"

// The harness itself: greeting, login, a posted article reaches the group.
func TestHarnessPostAndRead(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	login(c, user, pass)
	c.ExpectPost(240, testArticle(t, "<harness-1@e2e.test>", group).Bytes())
	c.ExpectCmd(211, "GROUP %s", group)
	c.ExpectCmd(220, "ARTICLE <harness-1@e2e.test>")
	if got := c.ReadDotLines(); len(got) == 0 {
		t.Fatal("empty article")
	}
}
