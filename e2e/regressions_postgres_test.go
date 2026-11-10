//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
	"github.com/jackc/pgx/v5"

	"openusenet/internal/auth"
	"openusenet/internal/config"
	"openusenet/internal/store"
)

// startPostgresEnv starts a server like startEnv, but on the PostgreSQL store named by
// OPENUSENET_TEST_POSTGRES (a DSN, URL or key=value form), in a fresh schema that is dropped
// after the test (search_path). Without the variable the test is skipped. It returns the
// listener address.
func startPostgresEnv(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("OPENUSENET_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("OPENUSENET_TEST_POSTGRES not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("ous_e2e_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	// Registered first, so it runs last: after the store is closed.
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}

	st, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.EnsureGroup(context.Background(), group, "e2e group "+group, "y"); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(context.Background(), store.User{Username: user, PasswordHash: hash, Role: store.RoleUser, CanPost: true}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Server.Hostname = pathhost
	cfg.Server.Pathhost = pathhost
	cfg.Listen.HTTP = "-"
	off := false
	cfg.Watchdog.Enabled = &off
	addr, _, done := serve(t, ctx, &cfg, st)
	// Stop the server before the store closes (cleanups run last in, first out).
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return addr
}

// O-13: the articles table kept headers and body as PostgreSQL TEXT, which refuses bytes that
// are not UTF-8 and NUL, so an 8-bit article (Latin-1 text, yEnc) got 436 on IHAVE (offered
// again forever) and 403 on POST. Only reproducible on PostgreSQL; the memory store keeps
// bytes. Right: the article is stored and served byte for byte, by IHAVE and by POST.
func TestRegressionO13EightBitArticlesPostgres(t *testing.T) {
	knownIssue(t, "O-13", "8-bit articles cannot be stored in PostgreSQL TEXT columns")
	addr := startPostgresEnv(t)
	c := client.Dial(t, addr)
	login(c, user, pass)

	bodies := map[string]string{
		"latin1": "caf\xe9 noir, na\xefve, \xe0 la carte\r\n",
		"nul":    "before\x00after\r\n",
	}
	// The headers column was TEXT too: the Latin-1 case also carries an 8-bit header line,
	// which must come back from HEAD unchanged. (No NUL in a header: Parse refuses that.)
	extra := map[string]string{
		"latin1": "X-Octets: caf\xe9 \xe0 la carte",
	}
	checkHead := func(msgid, how, name string) {
		t.Helper()
		x := extra[name]
		if x == "" {
			return
		}
		if head := rawBlock(c, 221, "HEAD "+msgid); !strings.Contains(head, "\r\n"+x+"\r\n") {
			t.Errorf("HEAD %s (%s) lost the 8-bit header %q:\n%q", msgid, how, x, head)
		}
	}
	for _, name := range []string{"latin1", "nul"} {
		body := bodies[name]
		var xh []string
		if x := extra[name]; x != "" {
			xh = []string{x}
		}

		msgid := "<o13-ihave-" + name + "@e2e.test>"
		head := strings.Join(append([]string{
			"Path: e2e-peer!not-for-mail",
			"From: E2E Tester <e2e@example.invalid>",
			"Newsgroups: " + group,
			"Subject: 8-bit by IHAVE (" + name + ")",
			"Date: " + nowDate(),
			"Message-ID: " + msgid,
		}, xh...), "\r\n") + "\r\n"
		c.ExpectCmd(335, "IHAVE %s", msgid)
		c.MustSend(dotStuff(head + "\r\n" + body))
		if line := readStatus(t, c); !strings.HasPrefix(line, "235 ") {
			t.Errorf("IHAVE %s: %q, want 235", msgid, line)
		} else {
			if got, want := rawBlock(c, 222, "BODY "+msgid), dotStuff(body); got != want {
				t.Errorf("BODY %s (IHAVE)\n got %q\nwant %q", msgid, got, want)
			}
			checkHead(msgid, "IHAVE", name)
		}

		msgid = "<o13-post-" + name + "@e2e.test>"
		c.ExpectCmd(340, "POST")
		post := append([]string{
			"From: E2E Tester <e2e@example.invalid>",
			"Newsgroups: " + group,
			"Subject: 8-bit by POST (" + name + ")",
			"Message-ID: " + msgid,
		}, xh...)
		c.MustSend(dotStuff(strings.Join(append(post, "", body), "\r\n")))
		if line := readStatus(t, c); !strings.HasPrefix(line, "240 ") {
			t.Errorf("POST %s: %q, want 240", msgid, line)
		} else {
			if got, want := rawBlock(c, 222, "BODY "+msgid), dotStuff(body); got != want {
				t.Errorf("BODY %s (POST)\n got %q\nwant %q", msgid, got, want)
			}
			checkHead(msgid, "POST", name)
		}
	}
}
