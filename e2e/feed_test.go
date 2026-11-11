//go:build e2e

package e2e

// Outgoing feeder tests: articles POSTed to OpenUsenetServer are offered to fake transit
// peers (nntp-testkit's fakeserver.NewPeer, a PeerHandler with the knobs each test needs).
//
// The feeder delivers asynchronously (Offer enqueues, then a drain goroutine connects), so
// every test waits for a count with testkit.WaitFor and then asserts exact counts. A test that
// expects an article NOT to be offered posts a control article after it and waits for that one:
// POST sends its 240 and then enqueues (cmdPost calls offer right after the reply), and one
// session handles its commands in order, so the excluded article is queued (or not) before the
// control POST is read. Once the control article has arrived and the queue is empty, the
// excluded one can no longer come.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/article"
	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"

	"openusenet/internal/store"
)

// feedWait is the bound for a feeder delivery: generous, as the drain is asynchronous; a
// passing test never waits on it.
func feedWait() time.Duration { return max(testkit.Deadline(), 30*time.Second) }

// newPeerServer starts a fake transit peer (the kit's PeerHandler) with the default greeting.
func newPeerServer(t *testing.T, o *fakeserver.PeerOptions) *fakeserver.Server {
	t.Helper()
	return fakeserver.NewPeer(t, fakeserver.Options{}, o)
}

// peerOf returns an enabled store.Peer that points at srv and takes every group.
func peerOf(name string, srv *fakeserver.Server) store.Peer {
	return store.Peer{Name: name, Host: srv.Host(), Port: srv.Port(), Patterns: "*", Enabled: true}
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

// msgIDsOf returns the Message-IDs of the articles srv received, in arrival order.
func msgIDsOf(srv *fakeserver.Server) []string {
	var ids []string
	for _, raw := range srv.ArticlesRaw() {
		ids = append(ids, article.ParseDotBlock(raw).MessageID())
	}
	return ids
}

// checkArticle compares an article the peer received (an element of ArticlesRaw: the bytes the
// feeder wrote, still dot-stuffed, without the terminator) with the one posted: same
// Message-ID, our path host prepended to the posted Path, and the body byte for byte as it went
// over the wire: the posted body dot-stuffed (client.StuffBytes), CRLF line ends included.
func checkArticle(t *testing.T, raw []byte, posted *article.Article) {
	t.Helper()
	recv := article.ParseDotBlock(raw)
	if got, want := recv.MessageID(), posted.MessageID(); got != want {
		t.Errorf("Message-ID = %q, want %q", got, want)
	}
	// The server prepends its own hop and keeps the posted Path behind it.
	if got, want := recv.Get("Path"), posted.Clone().PrependPath(pathhost).Get("Path"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	_, gotBody, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
	wantBody := bytes.TrimSuffix(client.StuffBytes(posted.Body), []byte(".\r\n"))
	if !bytes.Equal(gotBody, wantBody) {
		t.Errorf("body on the wire = %q, want %q", gotBody, wantBody)
	}
}

// bodyArticle is testArticle with body lines that need dot-stuffing on the wire.
func bodyArticle(t *testing.T, msgid string) *article.Article {
	a := testArticle(t, msgid, group)
	return a.SetBodyLines(append(a.BodyLines(), ".leading dot", "..two dots", ".", "last line")...)
}

func TestFeedStreamingCheckTakethis(t *testing.T) {
	e := startEnv(t)
	srv := newPeerServer(t, &fakeserver.PeerOptions{})
	e.addPeer(peerOf("streampeer", srv))

	c := e.dial()
	login(c, user, pass)
	art := bodyArticle(t, "<feed-stream-1@e2e.test>")
	c.ExpectPost(240, art.Bytes())

	e.waitDelivered(t, srv, 1)
	arts := srv.ArticlesRaw()
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
	art := bodyArticle(t, "<feed-ihave-1@e2e.test>")
	c.ExpectPost(240, art.Bytes())

	e.waitDelivered(t, srv, 1)
	arts := srv.ArticlesRaw()
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
	c.ExpectPost(240, testArticle(t, "<feed-path-token@e2e.test>", group).Set("Path", "hub.example!origin.example!not-for-mail").Bytes())
	c.ExpectPost(240, testArticle(t, "<feed-path-name@e2e.test>", group).Set("Path", "feedpeer!origin.example!not-for-mail").Bytes())
	c.ExpectPost(240, testArticle(t, "<feed-path-control@e2e.test>", group).Bytes())

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
	c.ExpectPost(240, testArticle(t, "<feed-pat-test@e2e.test>", group).Bytes())
	c.ExpectPost(240, testArticle(t, "<feed-pat-other@e2e.test>", other).Bytes())

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
	c.ExpectPost(240, testArticle(t, had, group).Bytes())
	c.ExpectPost(240, testArticle(t, "<feed-have-control@e2e.test>", group).Bytes())

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
		c.ExpectPost(240, testArticle(t, fmt.Sprintf("<feed-persist-%d@e2e.test>", i), group).Bytes())
	}
	e.waitDelivered(t, srv, n)
	if got := len(srv.Articles()); got != n {
		t.Fatalf("peer got %d articles, want %d", got, n)
	}
	if a, limit := srv.Accepted(), n/5; a > limit {
		t.Errorf("%d articles used %d connections (MaxOpen %d), want at most %d", n, a, srv.MaxOpen(), limit)
	}
}

// O-18: a peer whose CAPABILITIES fails (Diablo: 500) but that streams after MODE STREAM
// (203) should get CHECK/TAKETHIS; the feeder never tries MODE STREAM and falls back to IHAVE.
func TestFeedModeStreamFallback(t *testing.T) {
	knownIssue(t, "O-18", "no MODE STREAM fallback when CAPABILITIES fails")
	e := startEnv(t)
	// Diablo's feeder port: CAPABILITIES is unknown (500), MODE STREAM gives 203, then
	// CHECK/TAKETHIS (and IHAVE) as usual.
	srv := fakeserver.NewPeer(t, fakeserver.Options{Greeting: "200 diablo-like server ready"}, &fakeserver.PeerOptions{
		CapabilitiesReply: []string{"500 Syntax error or bad command"},
		ModeStreamReply:   []string{"203 StreamOK."},
	})
	e.addPeer(peerOf("diablopeer", srv))

	c := e.dial()
	login(c, user, pass)
	art := bodyArticle(t, "<feed-modestream-1@e2e.test>")
	c.ExpectPost(240, art.Bytes())

	e.waitDelivered(t, srv, 1)
	checkArticle(t, srv.ArticlesRaw()[0], art)
	n := srv.Counts()
	if !slices.Contains(srv.Lines(), "MODE STREAM") || n["TAKETHIS"] != 1 || n["IHAVE"] != 0 {
		t.Errorf("MODE=%d CHECK=%d TAKETHIS=%d IHAVE=%d, want MODE STREAM then CHECK/TAKETHIS and no IHAVE (lines %q)",
			n["MODE"], n["CHECK"], n["TAKETHIS"], n["IHAVE"], srv.Lines())
	}
}

// O-20: every accepted article starts its own drain goroutine, so the connections to
// one peer run in parallel without a limit. With every connection held open until the test
// releases it (without the hold, short connections hardly overlap and the bound proves
// nothing), MaxOpen must stay at a small bound.
func TestFeedParallelBound(t *testing.T) {
	knownIssue(t, "O-20", "unbounded parallel feeder connections to one peer")
	e := startEnv(t)
	// Every connection the feeder opens waits for its greeting, and stays open, until the test
	// closes hold (HoldGreeting; srv.Close at cleanup drops connections still held).
	hold := make(chan struct{})
	srv := fakeserver.NewPeer(t, fakeserver.Options{}, &fakeserver.PeerOptions{HoldGreeting: hold})
	e.addPeer(peerOf("parallelpeer", srv))

	const n, bound = 16, 4
	c := e.dial()
	login(c, user, pass)
	for i := range n {
		c.ExpectPost(240, testArticle(t, fmt.Sprintf("<feed-o20-%d@e2e.test>", i), group).Bytes())
	}
	// Hold the connections until more than bound are open, or for a hold window when a
	// bounded feeder never opens that many. The window is not an assertion (a bounded feeder
	// passes however long it lasts); it stays below the feeder's 15 s read timeout (and the
	// kit's Deadline, after which a held connection is dropped).
	testkit.WaitFor(t, feedWait(), "a first held connection", func() bool { return srv.Open() >= 1 })
	until := time.Now().Add(5 * time.Second)
	for srv.Open() <= bound && time.Now().Before(until) {
		time.Sleep(50 * time.Millisecond)
	}
	held := srv.Open()
	close(hold)

	e.waitDelivered(t, srv, n)
	if m := srv.MaxOpen(); m > bound {
		t.Errorf("MaxOpen() = %d (held at once: %d), want at most %d parallel connections to one peer (Accepted %d, %d articles)",
			m, held, bound, srv.Accepted(), n)
	}
}

// O-21: a drain claims up to 32 items with a 2-minute lease and delivers them one after
// the other. A batch slower than the lease is claimed again by the worker's next drain, and
// the same articles are sent twice. One article to 30 peers puts 30 items into one claim; each
// peer greets after 5 s, so the batch takes about 150 s. Each peer must be offered the
// article exactly once.
func TestFeedLeaseNoDuplicates(t *testing.T) {
	knownIssue(t, "O-21", "a batch slower than the 2-minute claim lease is sent twice")
	if os.Getenv("OUS_E2E_SLOW") != "1" {
		t.Skip("O-21 needs about 3 minutes (the 2-minute claim lease is hardcoded); set OUS_E2E_SLOW=1 to run (and go test -timeout 15m)")
	}
	e := startEnv(t)
	const peers, delay = 30, 5 * time.Second
	var srvs []*fakeserver.Server
	for i := range peers {
		srv := fakeserver.NewPeer(t, fakeserver.Options{}, &fakeserver.PeerOptions{GreetingDelay: delay})
		srvs = append(srvs, srv)
		e.addPeer(peerOf(fmt.Sprintf("leasepeer%02d", i), srv))
	}

	c := e.dial()
	login(c, user, pass)
	const id = "<feed-o21-1@e2e.test>"
	c.ExpectPost(240, testArticle(t, id, group).Bytes())

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
