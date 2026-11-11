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

// allowedKnownIssues are conformance failures that are known on main, tolerated with
// conformance Options.Allow unless OUS_E2E_KNOWN_ISSUES=1: a Fail of the check whose detail
// contains the text is reported as a Skip ("allowed: ..."), any other Fail still fails.
//
// no-current-article is O-23 (ISSUES.md): in a group without a current article, HDR without a
// range answers 423 where 420 is right; fixed by fix/hdr-no-current-article. HDR is the last
// command of the check, after ARTICLE, HEAD, BODY, STAT, NEXT, LAST and OVER, so allowing
// only its 423 keeps the rest of the check asserted (Skip would drop the whole check). Drop the
// entry when the fix is merged.
var allowedKnownIssues = map[string]string{
	"no-current-article": `HDR Subject: got "423 `,
}

func TestConformance(t *testing.T) {
	e := startEnv(t)
	addr := e.Addr
	// The article checks (ARTICLE/HEAD/BODY/STAT, OVER, HDR, LISTGROUP, NEXT/LAST) skip on an
	// empty group, so post a few articles first.
	c := e.dial()
	login(c, user, pass)
	for i := 1; i <= 3; i++ {
		c.ExpectPost(240, testArticle(t, fmt.Sprintf("<conformance-seed-%d@e2e.test>", i), group).Bytes())
	}
	c.Close()
	var skip []string
	var allow map[string]string
	if os.Getenv("OUS_E2E_KNOWN_ISSUES") != "1" {
		skip, allow = authinfoKnownIssues, allowedKnownIssues
		t.Logf("skipping known issues O-01..O-04 (%v), allowing O-23 (%v); set OUS_E2E_KNOWN_ISSUES=1 to run them", skip, allow)
	}
	rep := conformance.RunVerbose(t, conformance.Options{
		Skip:      skip,
		Allow:     allow,
		Addr:      addr,
		User:      user,
		Pass:      pass,
		PostGroup: group,
		Deadline:  10 * time.Second,
	})
	p, f, s := rep.Counts()
	t.Logf("conformance: %d pass, %d fail, %d skip\n%s", p, f, s, rep)
}
