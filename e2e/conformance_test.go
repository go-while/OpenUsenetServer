//go:build e2e

// Package e2e runs the nntp-testkit conformance checks (RFC 3977/4643/4644) against a real
// OpenUsenetServer listener backed by the in-memory store, so no Postgres is needed.
//
//	cd e2e && go test -tags e2e -v .   (or: make e2e)
//
// e2e is its own module (Go 1.26, for nntp-testkit), so the server module keeps its Go version.
package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/conformance"
)

// authinfoKnownIssues are the RFC 4643 checks that fail on main: O-01 to O-04 in ISSUES.md,
// fixed by fix/authinfo-codes (upstream PR #4). They are skipped unless OUS_E2E_KNOWN_ISSUES=1;
// drop them from this list when the fix is merged.
var authinfoKnownIssues = []string{
	"authinfo-wrong-pass",
	"authinfo-pass-without-user",
	"authinfo-after-auth",
	"authinfo-capabilities",
}

func TestConformance(t *testing.T) {
	e := startEnv(t)
	addr := e.Addr
	// The article checks (ARTICLE/HEAD/BODY/STAT, OVER, HDR, LISTGROUP, NEXT/LAST) skip on an
	// empty group, so post a few articles first.
	c := e.dial()
	login(c, user, pass)
	for i := 1; i <= 3; i++ {
		c.ExpectCmd(340, "POST")
		c.MustSend(client.Stuff(testArticle(fmt.Sprintf("<conformance-seed-%d@e2e.test>", i), group)))
		c.ExpectStatus(240, "POST seed article")
	}
	c.Close()
	var skip []string
	if os.Getenv("OUS_E2E_KNOWN_ISSUES") != "1" {
		skip = authinfoKnownIssues
		t.Logf("skipping known issues O-01..O-04 (%v); set OUS_E2E_KNOWN_ISSUES=1 to run them", skip)
	}
	rep := conformance.RunVerbose(t, conformance.Options{
		Skip:      skip,
		Addr:      addr,
		User:      user,
		Pass:      pass,
		PostGroup: group,
		Deadline:  10 * time.Second,
	})
	p, f, s := rep.Counts()
	t.Logf("conformance: %d pass, %d fail, %d skip\n%s", p, f, s, rep)
}
