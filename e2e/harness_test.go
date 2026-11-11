//go:build e2e

package e2e

// Harness for the e2e tests: a real OpenUsenetServer listener on 127.0.0.1:0 in this process,
// backed by the in-memory store (no Postgres), and nntp-testkit clients and fake peers.
//
// The server module keeps its own unit tests with local helpers (internal/*, Go 1.25, no kit
// dependency); the tests here are the kit-based counterparts and the regression tests for the
// findings in ISSUES.md.

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/article"
	"github.com/go-while/nntp-testkit/client"

	"openusenet/internal/auth"
	"openusenet/internal/config"
	"openusenet/internal/server"
	"openusenet/internal/store"
)

const (
	group    = "local.test"
	user     = "e2euser"
	pass     = "e2e-secret-pass"
	pathhost = "e2e.test"
	testFrom = "E2E Tester <e2e@example.invalid>"
)

// env is one running server with its store.
type env struct {
	t     *testing.T
	Addr  string
	Store *store.Memory
	Cfg   config.Config
	Srv   *server.Server
}

// option changes the configuration before the server starts.
type option func(*config.Config)

// startEnv starts a server with group local.test and the posting user e2euser. The load
// watchdog is off, so a busy test host does not make the server refuse articles (441).
func startEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st := store.NewMemory()
	e := &env{t: t, Store: st}
	e.addGroup(group, "y")
	e.addUser(user, pass, true)

	cfg := config.Defaults()
	cfg.Server.Hostname = pathhost
	cfg.Server.Pathhost = pathhost
	cfg.Listen.NNTP = "127.0.0.1:0"
	cfg.Listen.HTTP = "-"
	off := false
	cfg.Watchdog.Enabled = &off
	for _, o := range opts {
		o(&cfg)
	}
	e.Addr, e.Srv, _ = serve(t, ctx, &cfg, st)
	e.Cfg = cfg
	return e
}

// serve starts srv on a free port of 127.0.0.1 and returns its address once it accepts
// connections, and a channel closed when ListenAndServe returns. It does not poll
// server.Addr(): ListenAndServe stores its listener without synchronisation, so reading it
// from the test goroutine is a data race (go test -race reports it). A port taken between
// FreePort and the server's listen makes ListenAndServe fail at once; then it tries another.
func serve(t *testing.T, ctx context.Context, cfg *config.Config, st store.Store) (string, *server.Server, <-chan struct{}) {
	t.Helper()
	var lastErr error
	for range 5 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testkit.FreePort(t)))
		cfg.Listen.NNTP = addr
		srv := server.New(*cfg, st, nil, nil)
		errc := make(chan error, 1)
		done := make(chan struct{})
		go func() { errc <- srv.ListenAndServe(ctx); close(done) }()
		deadline := time.Now().Add(10 * time.Second)
		for {
			select {
			case lastErr = <-errc:
			default:
				if testkit.Listening(addr) {
					return addr, srv, done
				}
				if time.Now().After(deadline) {
					t.Fatalf("server did not start listening on %s", addr)
				}
				time.Sleep(10 * time.Millisecond)
				continue
			}
			break
		}
	}
	t.Fatalf("server did not start: %v", lastErr)
	return "", nil, nil
}

func (e *env) addGroup(name, status string) {
	e.t.Helper()
	if err := e.Store.EnsureGroup(context.Background(), name, "e2e group "+name, status); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) addUser(name, password string, canPost bool) {
	e.t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.Store.CreateUser(context.Background(), store.User{Username: name, PasswordHash: hash, Role: store.RoleUser, CanPost: canPost}); err != nil {
		e.t.Fatal(err)
	}
}

// addPeer stores an outgoing feed peer; the feeder reads peers from the store on each offer.
func (e *env) addPeer(p store.Peer) *store.Peer {
	e.t.Helper()
	got, err := e.Store.CreatePeer(context.Background(), p)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// dial connects a strict kit client (greeting read, hang guard set, closed at cleanup).
func (e *env) dial() *client.Conn {
	e.t.Helper()
	return client.Dial(e.t, e.Addr)
}

// login authenticates c as name/password (381, then 281).
func login(c *client.Conn, name, password string) {
	c.ExpectCmd(381, "AUTHINFO USER %s", name)
	c.ExpectCmd(281, "AUTHINFO PASS %s", password)
}

// knownIssue skips a test that pins a finding still open on main (ISSUES.md id), unless
// OUS_E2E_KNOWN_ISSUES=1: then it runs, and fails until the fix is merged. Remove the call
// when the fix lands.
func knownIssue(t *testing.T, id, what string) {
	t.Helper()
	if os.Getenv("OUS_E2E_KNOWN_ISSUES") != "1" {
		t.Skipf("known issue %s (%s); set OUS_E2E_KNOWN_ISSUES=1 to run", id, what)
	}
}

// testArticle returns the article the e2e tests offer by IHAVE or TAKETHIS (and post), built
// with the kit's article.New: Path "e2e-peer!not-for-mail", From, Newsgroups, Subject
// "e2e <msgid without brackets>", Message-ID, Date (now, UTC), then the extra header lines
// (each written as given), the empty line and the body lines "e2e body line 1" and
// "e2e body line 2". Edit the result (Set, Insert, SetBodyLines ...) and send a.Bytes() with
// c.ExpectIhave, c.ExpectTakethis or c.ExpectPost.
func testArticle(tb testing.TB, msgid, newsgroups string, extra ...string) *article.Article {
	tb.Helper()
	return article.New(tb, article.Options{
		Newsgroups: newsgroups,
		PathHost:   "e2e-peer",
		From:       testFrom,
		Subject:    "e2e " + strings.Trim(msgid, "<>"),
		MessageID:  msgid,
		Extra:      extra,
		Body:       []string{"e2e body line 1", "e2e body line 2"},
	})
}

// testPost returns an article as a newsreader posts it to group local.test: From,
// Newsgroups, Subject, Message-ID, the empty line and the body lines; no Path and no Date (the
// server adds them). msgid "" leaves out the Message-ID as well, so the server injects one.
func testPost(tb testing.TB, msgid, subject string, body ...string) *article.Article {
	tb.Helper()
	omit := []string{"Path", "Date"}
	if msgid == "" {
		omit = append(omit, "Message-ID")
	}
	return article.New(tb, article.Options{
		Newsgroups: group,
		From:       testFrom,
		Subject:    subject,
		MessageID:  msgid,
		Omit:       omit,
		Body:       body,
	})
}
