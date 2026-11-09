//go:build e2e

// Package e2e runs the nntp-testkit conformance checks (RFC 3977/4643/4644) against a real
// OpenUsenetServer listener backed by the in-memory store, so no Postgres is needed.
//
//	cd e2e && go test -tags e2e -v .   (or: make e2e)
//
// e2e is its own module (Go 1.26, for nntp-testkit), so the server module keeps its Go version.
package e2e

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/conformance"

	"openusenet/internal/auth"
	"openusenet/internal/config"
	"openusenet/internal/server"
	"openusenet/internal/store"
)

const (
	group = "local.test"
	user  = "e2euser"
	pass  = "e2e-secret-pass"
)

func startServer(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st := store.NewMemory()
	if err := st.EnsureGroup(ctx, group, "Local test group", "y"); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, store.User{Username: user, PasswordHash: hash, Role: store.RoleUser, CanPost: true}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Server.Hostname = "e2e.test"
	cfg.Server.Pathhost = "e2e.test"
	cfg.Listen.NNTP = "127.0.0.1:0"
	cfg.Listen.HTTP = "-"
	srv := server.New(cfg, st, nil, nil)
	go func() { _ = srv.ListenAndServe(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != nil {
			return a.(*net.TCPAddr).String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start listening")
	return ""
}

func TestConformance(t *testing.T) {
	addr := startServer(t)
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
