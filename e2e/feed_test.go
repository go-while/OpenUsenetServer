//go:build e2e

package e2e

// Outgoing feeder tests: articles POSTed to OpenUsenetServer are offered to fake transit
// peers (nntp-testkit's fakeserver.PeerHandler, or a local handler where the kit has none).
//
// The feeder delivers asynchronously (Offer enqueues, then a drain goroutine connects), so
// every test waits for a count with testkit.WaitFor and then asserts exact counts. A test that
// expects an article NOT to be offered posts a control article after it and waits for that one:
// the decision to enqueue is made synchronously in POST (before the 240), so once the control
// article has arrived and the queue is empty, the excluded one can no longer come.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"

	"openusenet/internal/store"
)

// feedWait is the bound for a feeder delivery: generous, as the drain is asynchronous; a
// passing test never waits on it.
func feedWait() time.Duration { return max(testkit.Deadline(), 30*time.Second) }

// newPeerServer starts a fake transit peer with the kit's PeerHandler.
func newPeerServer(t *testing.T, o *fakeserver.PeerOptions) *fakeserver.Server {
	t.Helper()
	return fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(o)})
}

// peerOf returns an enabled store.Peer that points at srv and takes every group.
func peerOf(name string, srv *fakeserver.Server) store.Peer {
	return store.Peer{Name: name, Host: srv.Host(), Port: srv.Port(), Patterns: "*", Enabled: true}
}

// post sends art with POST on c and expects 240.
func post(c *client.Conn, art []string) {
	c.ExpectCmd(340, "POST")
	c.MustSend(client.Stuff(art))
	c.ExpectStatus(240, "POST")
}

// withPath returns art with its Path header replaced by path.
func withPath(art []string, path string) []string {
	out := slices.Clone(art)
	for i, l := range out {
		if strings.HasPrefix(l, "Path: ") {
			out[i] = "Path: " + path
		}
	}
	return out
}

// queueDepth is the number of items in the feed queue (claimed or not).
func (e *env) queueDepth() int {
	e.t.Helper()
	st, err := e.Store.FeedQueueStats(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return st.Depth
}

// waitDelivered waits until srv holds n articles and the feed queue is empty.
func (e *env) waitDelivered(t *testing.T, srv *fakeserver.Server, n int) {
	t.Helper()
	testkit.WaitFor(t, feedWait(), fmt.Sprintf("%d article(s) at the peer and an empty feed queue", n), func() bool {
		return len(srv.Articles()) >= n && e.queueDepth() == 0
	})
}

// unstuff removes the dot-stuffing of wire lines (RFC 3977 3.1.1).
func unstuff(wire []string) []string {
	out := make([]string, len(wire))
	for i, l := range wire {
		out[i] = strings.TrimPrefix(l, ".")
	}
	return out
}

// splitArticle splits article lines at the first empty line.
func splitArticle(lines []string) (head, body []string) {
	if i := slices.Index(lines, ""); i >= 0 {
		return lines[:i], lines[i+1:]
	}
	return lines, nil
}

// header returns the value of the first header name in head ("" if absent).
func header(head []string, name string) string {
	for _, l := range head {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// msgIDsOf returns the Message-IDs of the articles srv received, in arrival order.
func msgIDsOf(srv *fakeserver.Server) []string {
	var ids []string
	for _, a := range srv.Articles() {
		h, _ := splitArticle(unstuff(a))
		ids = append(ids, header(h, "Message-ID"))
	}
	return ids
}

// checkArticle compares a received article with the one posted: same Message-ID, Path with
// our path host prepended, and the body byte for byte (after dot-unstuffing).
func checkArticle(t *testing.T, wire, posted []string) {
	t.Helper()
	gotHead, gotBody := splitArticle(unstuff(wire))
	wantHead, wantBody := splitArticle(posted)
	if got, want := header(gotHead, "Message-ID"), header(wantHead, "Message-ID"); got != want {
		t.Errorf("Message-ID = %q, want %q", got, want)
	}
	if p := header(gotHead, "Path"); !strings.HasPrefix(p, pathhost+"!") {
		t.Errorf("Path = %q, want it to start with %q", p, pathhost+"!")
	}
	if !slices.Equal(gotBody, wantBody) {
		t.Errorf("body = %q, want %q", gotBody, wantBody)
	}
}

// bodyArticle is testArticle with body lines that need dot-stuffing on the wire.
func bodyArticle(msgid string) []string {
	return append(testArticle(msgid, group), ".leading dot", "..two dots", ".", "last line")
}

func TestFeedStreamingCheckTakethis(t *testing.T) {
	e := startEnv(t)
	srv := newPeerServer(t, &fakeserver.PeerOptions{})
	e.addPeer(peerOf("streampeer", srv))

	c := e.dial()
	login(c, user, pass)
	art := bodyArticle("<feed-stream-1@e2e.test>")
	post(c, art)

	e.waitDelivered(t, srv, 1)
	arts := srv.Articles()
	if len(arts) != 1 {
		t.Fatalf("peer got %d articles, want 1", len(arts))
	}
	checkArticle(t, arts[0], art)
	n := srv.Counts()
	if n["CHECK"] != 1 || n["TAKETHIS"] != 1 || n["IHAVE"] != 0 {
		t.Errorf("CHECK=%d TAKETHIS=%d IHAVE=%d, want 1 1 0 (counts %v)", n["CHECK"], n["TAKETHIS"], n["IHAVE"], n)
	}
	if !slices.Contains(srv.Lines(), "CHECK <feed-stream-1@e2e.test>") {
		t.Errorf("no CHECK <feed-stream-1@e2e.test> in %q", srv.Lines())
	}
	if a := srv.Accepted(); a != 1 {
		t.Errorf("Accepted() = %d, want 1", a)
	}
}

func TestFeedIHAVEWithoutStreaming(t *testing.T) {
	e := startEnv(t)
	srv := newPeerServer(t, &fakeserver.PeerOptions{NoStream: true})
	e.addPeer(peerOf("ihavepeer", srv))

	c := e.dial()
	login(c, user, pass)
	art := bodyArticle("<feed-ihave-1@e2e.test>")
	post(c, art)

	e.waitDelivered(t, srv, 1)
	arts := srv.Articles()
	if len(arts) != 1 {
		t.Fatalf("peer got %d articles, want 1", len(arts))
	}
	checkArticle(t, arts[0], art)
	n := srv.Counts()
	if n["IHAVE"] != 1 || n["CHECK"] != 0 || n["TAKETHIS"] != 0 {
		t.Errorf("IHAVE=%d CHECK=%d TAKETHIS=%d, want 1 0 0 (counts %v)", n["IHAVE"], n["CHECK"], n["TAKETHIS"], n)
	}
	if a := srv.Accepted(); a != 1 {
		t.Errorf("Accepted() = %d, want 1", a)
	}
}

// An article whose Path already holds the peer's path token, or (without the Ap flag) its
// name, is not offered to that peer.
func TestFeedSkipsPeerInPath(t *testing.T) {
	e := startEnv(t)
	srv := newPeerServer(t, &fakeserver.PeerOptions{})
	p := peerOf("feedpeer", srv)
	p.PathToken = "hub.example"
	e.addPeer(p)

	c := e.dial()
	login(c, user, pass)
	post(c, withPath(testArticle("<feed-path-token@e2e.test>", group), "hub.example!origin.example!not-for-mail"))
	post(c, withPath(testArticle("<feed-path-name@e2e.test>", group), "feedpeer!origin.example!not-for-mail"))
	ctl := testArticle("<feed-path-control@e2e.test>", group)
	post(c, ctl)

	e.waitDelivered(t, srv, 1)
	if ids := msgIDsOf(srv); !slices.Equal(ids, []string{"<feed-path-control@e2e.test>"}) {
		t.Errorf("peer got %q, want only the control article", ids)
	}
	if n := srv.Count("CHECK"); n != 1 {
		t.Errorf("CHECK count = %d, want 1 (lines %q)", n, srv.Lines())
	}
	if a := srv.Accepted(); a != 1 {
		t.Errorf("Accepted() = %d, want 1", a)
	}
}

// A peer whose Patterns exclude the group gets nothing of it; another peer that wants the
// group gets it.
func TestFeedPatternsExcludeGroup(t *testing.T) {
	e := startEnv(t)
	const other = "local.other"
	e.addGroup(other, "y")
	want := newPeerServer(t, &fakeserver.PeerOptions{})
	excl := newPeerServer(t, &fakeserver.PeerOptions{})
	pw := peerOf("wantpeer", want)
	pw.Patterns = group
	px := peerOf("exclpeer", excl)
	px.Patterns = "*,!" + group
	e.addPeer(pw)
	e.addPeer(px)

	c := e.dial()
	login(c, user, pass)
	post(c, testArticle("<feed-pat-test@e2e.test>", group))
	post(c, testArticle("<feed-pat-other@e2e.test>", other))

	e.waitDelivered(t, want, 1)
	e.waitDelivered(t, excl, 1)
	if ids := msgIDsOf(want); !slices.Equal(ids, []string{"<feed-pat-test@e2e.test>"}) {
		t.Errorf("peer %q got %q, want only the %s article", pw.Patterns, ids, group)
	}
	if ids := msgIDsOf(excl); !slices.Equal(ids, []string{"<feed-pat-other@e2e.test>"}) {
		t.Errorf("peer %q got %q, want only the %s article", px.Patterns, ids, other)
	}
	if n := excl.Count("CHECK"); n != 1 {
		t.Errorf("excluding peer: CHECK count = %d, want 1 (lines %q)", n, excl.Lines())
	}
	if n := want.Count("CHECK"); n != 1 {
		t.Errorf("wanting peer: CHECK count = %d, want 1 (lines %q)", n, want.Lines())
	}
}

// A peer that already has the article answers CHECK with 438: no TAKETHIS follows and the
// queue item is done (the empty queue means it is never offered again).
func TestFeedHaveNotResent(t *testing.T) {
	e := startEnv(t)
	const had = "<feed-have-1@e2e.test>"
	srv := newPeerServer(t, &fakeserver.PeerOptions{Have: map[string]bool{had: true}})
	e.addPeer(peerOf("havepeer", srv))

	c := e.dial()
	login(c, user, pass)
	post(c, testArticle(had, group))
	post(c, testArticle("<feed-have-control@e2e.test>", group))

	e.waitDelivered(t, srv, 1)
	testkit.WaitFor(t, feedWait(), "CHECK of both articles", func() bool { return srv.Count("CHECK") >= 2 })
	if ids := msgIDsOf(srv); !slices.Equal(ids, []string{"<feed-have-control@e2e.test>"}) {
		t.Errorf("peer got %q, want only the control article", ids)
	}
	n := srv.Counts()
	if n["CHECK"] != 2 || n["TAKETHIS"] != 1 || n["IHAVE"] != 0 {
		t.Errorf("CHECK=%d TAKETHIS=%d IHAVE=%d, want 2 1 0 (lines %q)", n["CHECK"], n["TAKETHIS"], n["IHAVE"], srv.Lines())
	}
	checks := 0
	for _, l := range srv.Lines() {
		if l == "CHECK "+had {
			checks++
		}
	}
	if checks != 1 {
		t.Errorf("CHECK %s sent %d times, want 1", had, checks)
	}
	if d := e.queueDepth(); d != 0 {
		t.Errorf("feed queue depth = %d, want 0", d)
	}
}

// O-17: the feeder opens one TCP connection per article. N articles to one peer should share
// a persistent connection (or a small pool), not use N connections.
func TestFeedPersistentConnection(t *testing.T) {
	knownIssue(t, "O-17", "one feeder connection per article")
	e := startEnv(t)
	srv := newPeerServer(t, &fakeserver.PeerOptions{})
	e.addPeer(peerOf("persistpeer", srv))

	const n = 20
	c := e.dial()
	login(c, user, pass)
	for i := range n {
		post(c, testArticle(fmt.Sprintf("<feed-persist-%d@e2e.test>", i), group))
	}
	e.waitDelivered(t, srv, n)
	if got := len(srv.Articles()); got != n {
		t.Fatalf("peer got %d articles, want %d", got, n)
	}
	if a, limit := srv.Accepted(), n/5; a > limit {
		t.Errorf("%d articles used %d connections (MaxOpen %d), want at most %d", n, a, srv.MaxOpen(), limit)
	}
}

// diabloPeer answers like Diablo's feeder port: CAPABILITIES is unknown (500), MODE STREAM
// gives 203, then CHECK/TAKETHIS (and IHAVE).
// TODO(nntp-testkit): PeerOptions knob for a peer without CAPABILITIES (or Faults.Override
// applied to PeerHandler) would replace this handler.
func diabloPeer(c *fakeserver.Conn, line string) ([]string, bool) {
	verb, arg, _ := strings.Cut(line, " ")
	switch strings.ToUpper(verb) {
	case "CAPABILITIES":
		return []string{"500 Syntax error or bad command"}, false
	case "MODE":
		if strings.EqualFold(arg, "STREAM") {
			return []string{"203 StreamOK."}, false
		}
	case "CHECK":
		return []string{"238 " + arg}, false
	case "TAKETHIS":
		if _, err := c.ReadDotBlock(); err != nil {
			return nil, true
		}
		return []string{"239 " + arg}, false
	case "IHAVE":
		if err := c.WriteLines("335 " + arg); err != nil {
			return nil, true
		}
		if _, err := c.ReadDotBlock(); err != nil {
			return nil, true
		}
		return []string{"235 " + arg}, false
	case "QUIT":
		return []string{"205 bye"}, true
	}
	return []string{"500 Syntax error or bad command"}, false
}

// O-18: a peer whose CAPABILITIES fails (Diablo: 500) but that streams after MODE STREAM
// (203) should get CHECK/TAKETHIS; the feeder never tries MODE STREAM and falls back to IHAVE.
func TestFeedModeStreamFallback(t *testing.T) {
	knownIssue(t, "O-18", "no MODE STREAM fallback when CAPABILITIES fails")
	e := startEnv(t)
	srv := fakeserver.New(t, fakeserver.Options{
		Greeting: "200 diablo-like server ready",
		Handler:  fakeserver.LineHandler(diabloPeer),
	})
	e.addPeer(peerOf("diablopeer", srv))

	c := e.dial()
	login(c, user, pass)
	art := bodyArticle("<feed-modestream-1@e2e.test>")
	post(c, art)

	e.waitDelivered(t, srv, 1)
	checkArticle(t, srv.Articles()[0], art)
	n := srv.Counts()
	if !slices.Contains(srv.Lines(), "MODE STREAM") || n["TAKETHIS"] != 1 || n["IHAVE"] != 0 {
		t.Errorf("MODE=%d CHECK=%d TAKETHIS=%d IHAVE=%d, want MODE STREAM then CHECK/TAKETHIS and no IHAVE (lines %q)",
			n["MODE"], n["CHECK"], n["TAKETHIS"], n["IHAVE"], srv.Lines())
	}
}

// gate holds connections until it is opened.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate  { return &gate{ch: make(chan struct{})} }
func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }
func (g *gate) wait() { <-g.ch }

// heldPeer wraps inner: each connection waits for g before its greeting (use with
// Options.NoGreeting), so every connection the feeder opens stays open until the test opens
// the gate. The kit's PeerHandler writes with WriteLines, so Faults.Delay does not apply to it.
// TODO(nntp-testkit): gated PeerHandler
func heldPeer(g *gate, inner func(*fakeserver.Conn)) func(*fakeserver.Conn) {
	return func(c *fakeserver.Conn) {
		g.wait()
		if err := c.WriteLines(fakeserver.DefaultGreeting); err != nil {
			return
		}
		inner(c)
	}
}

// feed-parallel: every accepted article starts its own drain goroutine, so the connections to
// one peer run in parallel without a limit. With every connection held open until the test
// releases it (without the hold, short connections hardly overlap and the bound proves
// nothing), MaxOpen must stay at a small bound.
func TestFeedParallelBound(t *testing.T) {
	knownIssue(t, "feed-parallel", "unbounded parallel feeder connections to one peer")
	e := startEnv(t)
	g := newGate()
	srv := fakeserver.New(t, fakeserver.Options{
		NoGreeting: true,
		Handler:    heldPeer(g, fakeserver.PeerHandler(&fakeserver.PeerOptions{})),
	})
	t.Cleanup(g.open) // runs before srv.Close (cleanups run last-in first-out)
	e.addPeer(peerOf("parallelpeer", srv))

	const n, bound = 16, 4
	c := e.dial()
	login(c, user, pass)
	for i := range n {
		post(c, testArticle(fmt.Sprintf("<feed-parallel-%d@e2e.test>", i), group))
	}
	// Hold the connections until more than bound are open, or for a hold window when a
	// bounded feeder never opens that many. The window is not an assertion (a bounded feeder
	// passes however long it lasts); it stays below the feeder's 15 s read timeout.
	testkit.WaitFor(t, feedWait(), "a first held connection", func() bool { return srv.Open() >= 1 })
	hold := time.Now().Add(5 * time.Second)
	for srv.Open() <= bound && time.Now().Before(hold) {
		time.Sleep(50 * time.Millisecond)
	}
	held := srv.Open()
	g.open()

	e.waitDelivered(t, srv, n)
	if m := srv.MaxOpen(); m > bound {
		t.Errorf("MaxOpen() = %d (held at once: %d), want at most %d parallel connections to one peer (Accepted %d, %d articles)",
			m, held, bound, srv.Accepted(), n)
	}
}

// delayedPeer wraps inner: each connection gets its greeting after d (use with
// Options.NoGreeting).
// TODO(nntp-testkit): gated PeerHandler (or Faults.Delay for the greeting of a Handler)
func delayedPeer(d time.Duration, inner func(*fakeserver.Conn)) func(*fakeserver.Conn) {
	return func(c *fakeserver.Conn) {
		time.Sleep(d)
		if err := c.WriteLines(fakeserver.DefaultGreeting); err != nil {
			return
		}
		inner(c)
	}
}

// feed-lease: a drain claims up to 32 items with a 2-minute lease and delivers them one after
// the other. A batch slower than the lease is claimed again by the worker's next drain, and
// the same articles are sent twice. One article to 30 peers puts 30 items into one claim; each
// peer greets after 5 s, so the batch takes about 150 s. Each peer must be offered the
// article exactly once.
func TestFeedLeaseNoDuplicates(t *testing.T) {
	knownIssue(t, "feed-lease", "a batch slower than the 2-minute claim lease is sent twice")
	if os.Getenv("OUS_E2E_SLOW") != "1" {
		t.Skip("feed-lease needs about 3 minutes (the 2-minute claim lease is hardcoded); set OUS_E2E_SLOW=1 to run (and go test -timeout 15m)")
	}
	e := startEnv(t)
	const peers, delay = 30, 5 * time.Second
	var srvs []*fakeserver.Server
	for i := range peers {
		srv := fakeserver.New(t, fakeserver.Options{
			NoGreeting: true,
			Handler:    delayedPeer(delay, fakeserver.PeerHandler(&fakeserver.PeerOptions{})),
		})
		srvs = append(srvs, srv)
		e.addPeer(peerOf(fmt.Sprintf("leasepeer%02d", i), srv))
	}

	c := e.dial()
	login(c, user, pass)
	const id = "<feed-lease-1@e2e.test>"
	post(c, testArticle(id, group))

	slow := max(testkit.Deadline(), 8*time.Minute)
	testkit.WaitFor(t, slow, "the article at every peer, an empty queue and no open feeder connection", func() bool {
		if e.queueDepth() != 0 {
			return false
		}
		for _, s := range srvs {
			if len(s.Articles()) < 1 || s.Open() != 0 {
				return false
			}
		}
		return true
	})
	offers, conns := 0, 0
	for i, s := range srvs {
		n := s.Count("CHECK") + s.Count("IHAVE")
		offers += n
		conns += s.Accepted()
		if n != 1 {
			t.Errorf("leasepeer%02d: offered %s %d times (CHECK %d, IHAVE %d, TAKETHIS %d, connections %d), want 1",
				i, id, n, s.Count("CHECK"), s.Count("IHAVE"), s.Count("TAKETHIS"), s.Accepted())
		}
	}
	if offers != peers {
		t.Errorf("%d offers over %d connections to %d peers, want %d offers", offers, conns, peers, peers)
	}
}
