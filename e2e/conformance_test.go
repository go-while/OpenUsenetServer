//go:build e2e

// Package e2e runs the nntp-testkit conformance checks (RFC 3977/4643/4644) against a real
// OpenUsenetServer listener backed by the in-memory store, so no Postgres is needed.
//
//	cd e2e && go test -tags e2e -v .   (or: make e2e)
//
// e2e is its own module (Go 1.26, for nntp-testkit), so the server module keeps its Go version.
package e2e

import (
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/conformance"
)

func TestConformance(t *testing.T) {
	addr := startEnv(t).Addr
	rep := conformance.RunVerbose(t, conformance.Options{
		Addr:      addr,
		User:      user,
		Pass:      pass,
		PostGroup: group,
		Deadline:  10 * time.Second,
	})
	p, f, s := rep.Counts()
	t.Logf("conformance: %d pass, %d fail, %d skip\n%s", p, f, s, rep)
}
