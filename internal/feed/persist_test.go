package feed

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"openusenet/internal/nntp"
	"openusenet/internal/store"
)

// closeFeeder closes the connections f keeps open for the next drain.
func closeFeeder(f *Feeder) { f.closeIdle() }

// forWindows runs fn in order (window 1) and pipelined (window 16).
func forWindows(t *testing.T, fn func(t *testing.T, window int)) {
	for _, w := range []int{1, 16} {
		t.Run(fmt.Sprintf("window=%d", w), func(t *testing.T) { fn(t, w) })
	}
}

// CHECKs are pipelined: with 32 articles, the peer holds its first reply and finds the
// other 15 CHECKs of the window already sent, and not more (the window bounds them).
func TestDrainPipelinesChecks(t *testing.T) {
	p := &feedPeer{firstCheck: 150 * time.Millisecond}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	t.Cleanup(func() { closeFeeder(f) })
	if f.window != 16 {
		t.Fatalf("default window %d, want 16", f.window)
	}

	const n = 32 // one claim
	enqueue(t, q, "pipe", n)
	f.drain(context.Background())

	conns, _, got := p.stats()
	if conns != 1 || got != n || q.depth() != 0 {
		t.Errorf("%d connections, %d articles, queue depth %d; want 1, %d, 0", conns, got, q.depth(), n)
	}
	if c, tk := p.count("CHECK"), p.count("TAKETHIS"); c != n || tk != n {
		t.Errorf("CHECK %d, TAKETHIS %d, want %d each", c, tk, n)
	}
	p.mu.Lock()
	ahead := append([]int(nil), p.ahead...)
	p.mu.Unlock()
	if len(ahead) != 1 || ahead[0] != f.window-1 {
		t.Errorf("command lines sent before the first reply: %v, want [%d]", ahead, f.window-1)
	}
}

// bigQueue serves articles with a body of size bytes.
type bigQueue struct {
	*fakeQueue
	size int
}

func (b *bigQueue) GetByMsgID(_ context.Context, msgid string) (*store.StoredArticle, error) {
	line := strings.Repeat("x", 998) + "\r\n"
	return &store.StoredArticle{MessageID: msgid, Headers: string(testHead(msgid)),
		Body: strings.Repeat(line, b.size/len(line))}, nil
}

// Large articles pipelined behind CHECKs do not dead-lock the connection: the peer's
// replies stay within the window while we write.
func TestDrainPipelinesLargeArticles(t *testing.T) {
	p := &feedPeer{}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	b := &bigQueue{fakeQueue: q, size: 1 << 20}
	f := newTestFeeder(q)
	f.peers, f.arts, f.timeout = b, b, 5*time.Second
	t.Cleanup(func() { closeFeeder(f) })

	const n = 24
	enqueue(t, q, "big", n)
	f.drain(context.Background())
	if conns, _, got := p.stats(); conns != 1 || got != n || q.depth() != 0 {
		t.Errorf("%d connections, %d articles, queue depth %d; want 1, %d, 0", conns, got, q.depth(), n)
	}
}

// A second drain within the idle time reuses the connection; after the idle timeout the
// feeder has closed it (with QUIT) and the next drain connects again.
func TestDrainReusesConnectionUntilIdle(t *testing.T) {
	p := &feedPeer{}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	f.idleFor = 300 * time.Millisecond
	t.Cleanup(func() { closeFeeder(f) })

	enqueue(t, q, "reuse-a", 3)
	f.drain(context.Background())
	enqueue(t, q, "reuse-b", 3)
	f.drain(context.Background())
	if conns, open, got := p.stats(); conns != 1 || open != 1 || got != 6 {
		t.Fatalf("after two drains: %d connections (%d open), %d articles; want 1 (1 open), 6", conns, open, got)
	}

	waitFor(t, 3*time.Second, "the idle connection closed", func() bool {
		_, open, _ := p.stats()
		return open == 0
	})
	if c := p.count("QUIT"); c != 1 {
		t.Errorf("QUIT sent %d times, want 1 (at the idle timeout)", c)
	}
	enqueue(t, q, "reuse-c", 2)
	f.drain(context.Background())
	if conns, _, got := p.stats(); conns != 2 || got != 8 {
		t.Errorf("after the idle timeout: %d connections, %d articles; want 2, 8", conns, got)
	}
	if d := q.depth(); d != 0 {
		t.Errorf("queue depth %d, want 0", d)
	}
}

// CHECK and TAKETHIS replies are matched to their articles: 438 completes without
// TAKETHIS, 431 reschedules (FailFeed), 439 completes as refused, 239 as accepted.
func TestDrainMixedStreamingReplies(t *testing.T) { forWindows(t, testDrainMixedStreamingReplies) }

func testDrainMixedStreamingReplies(t *testing.T, window int) {
	ids := []string{"<mix-0@test>", "<mix-1@test>", "<mix-2@test>", "<mix-3@test>", "<mix-4@test>", "<mix-5@test>"}
	p := &feedPeer{
		check: map[string]string{
			ids[0]: "438 " + ids[0], // have it
			ids[2]: "431 " + ids[2], // later
			ids[4]: "438 " + ids[4],
		},
		take: map[string]string{ids[3]: "439 " + ids[3]}, // rejected after transfer
	}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	f.window = window
	t.Cleanup(func() { closeFeeder(f) })
	for _, id := range ids {
		_ = q.EnqueueFeed(context.Background(), 1, id)
	}
	f.drain(context.Background())

	if conns, _, _ := p.stats(); conns != 1 {
		t.Errorf("%d connections, want 1", conns)
	}
	if c, tk := p.count("CHECK"), p.count("TAKETHIS"); c != 6 || tk != 3 {
		t.Errorf("CHECK %d, TAKETHIS %d; want 6, 3 (none after 438/431)", c, tk)
	}
	for _, id := range []string{ids[1], ids[5]} {
		if p.received(id) != 1 {
			t.Errorf("%s not received", id)
		}
	}
	if e := q.failure(ids[2]); !strings.Contains(e, "431") {
		t.Errorf("431 article: failure %q, want a deferral", e)
	}
	for _, id := range []string{ids[0], ids[1], ids[3], ids[4], ids[5]} {
		if e := q.failure(id); e != "" {
			t.Errorf("%s failed with %q, want it done", id, e)
		}
	}
	if d := q.depth(); d != 1 {
		t.Errorf("queue depth %d, want 1 (the deferred article)", d)
	}
	if ok := f.Stats().OK; ok != 2 {
		t.Errorf("Stats.OK = %d, want 2 (the 239s)", ok)
	}
}

// A reply that names another message-id than the command it answers means the connection
// is out of step: the peer's items fail and the connection is dropped.
func TestDrainReplyOutOfStep(t *testing.T) { forWindows(t, testDrainReplyOutOfStep) }

func testDrainReplyOutOfStep(t *testing.T, window int) {
	p := &feedPeer{check: map[string]string{"<step-1@test>": "238 <other@test>"}}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	f.window = window
	t.Cleanup(func() { closeFeeder(f) })
	ids := enqueue(t, q, "step", 3)
	f.drain(context.Background())

	// In order, <step-0> was answered before the bad reply. Pipelined, its TAKETHIS reply
	// may still be unread then, and it is rescheduled (a second offer is refused).
	if window == 1 {
		if p.received(ids[0]) != 1 || q.failure(ids[0]) != "" {
			t.Errorf("%s: received %d, failure %q; want delivered", ids[0], p.received(ids[0]), q.failure(ids[0]))
		}
	}
	for _, id := range ids[1:] {
		if e := q.failure(id); !strings.Contains(e, "out of step") {
			t.Errorf("%s: failure %q, want out of step", id, e)
		}
	}
	waitFor(t, 3*time.Second, "the connection dropped", func() bool {
		_, open, _ := p.stats()
		return open == 0
	})
}

// A transport error in the middle of a batch fails the peer's remaining items (FailFeed)
// and drops the connection; the next drain connects again.
func TestDrainTransportErrorMidBatch(t *testing.T) { forWindows(t, testDrainTransportErrorMidBatch) }

func testDrainTransportErrorMidBatch(t *testing.T, window int) {
	p := &feedPeer{dropTake: "<mid-3@test>"}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	f.window = window
	t.Cleanup(func() { closeFeeder(f) })
	ids := enqueue(t, q, "mid", 8)
	f.drain(context.Background())

	for _, id := range ids[:3] {
		if p.received(id) != 1 || q.failure(id) != "" {
			t.Errorf("%s: received %d, failure %q; want delivered", id, p.received(id), q.failure(id))
		}
	}
	for _, id := range ids[3:] {
		if q.failure(id) == "" {
			t.Errorf("%s: no FailFeed after the connection broke", id)
		}
	}
	if d := q.depth(); d != 5 {
		t.Errorf("queue depth %d, want 5", d)
	}
	waitFor(t, 3*time.Second, "the broken connection gone", func() bool {
		_, open, _ := p.stats()
		return open == 0
	})

	next := enqueue(t, q, "after", 1)[0]
	f.drain(context.Background())
	if conns, _, _ := p.stats(); conns != 2 {
		t.Errorf("%d connections, want 2 (the next drain reconnects)", conns)
	}
	if p.received(next) != 1 {
		t.Errorf("%s not delivered after reconnecting", next)
	}
}

// A peer that closed the parked connection (idle timeout on its side) is noticed before
// the next drain uses it: that drain connects again and nothing fails.
func TestDrainPeerClosedParkedConnection(t *testing.T) {
	p := &feedPeer{idleClose: 100 * time.Millisecond}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	t.Cleanup(func() { closeFeeder(f) })

	enqueue(t, q, "closed-a", 2)
	f.drain(context.Background())
	waitFor(t, 3*time.Second, "the peer closing the idle connection", func() bool {
		_, open, _ := p.stats()
		return open == 0
	})
	ids := enqueue(t, q, "closed-b", 2)
	f.drain(context.Background())
	if conns, _, got := p.stats(); conns != 2 || got != 4 {
		t.Errorf("%d connections, %d articles; want 2, 4", conns, got)
	}
	for _, id := range ids {
		if e := q.failure(id); e != "" {
			t.Errorf("%s failed: %q", id, e)
		}
	}
}

// A parked connection that looks open but breaks on its first command (the peer dropped it
// without a word) costs nothing: no reply came on it, so the drain offers the same articles
// on a new connection.
func TestDrainParkedConnectionBreaksRetries(t *testing.T) {
	p := &feedPeer{dropIdle: 150 * time.Millisecond}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	t.Cleanup(func() { closeFeeder(f) })

	enqueue(t, q, "silent-a", 2)
	f.drain(context.Background())
	time.Sleep(300 * time.Millisecond)
	ids := enqueue(t, q, "silent-b", 3)
	f.drain(context.Background())
	if conns, _, got := p.stats(); conns != 2 || got != 5 {
		t.Errorf("%d connections, %d articles; want 2, 5", conns, got)
	}
	for _, id := range ids {
		if e := q.failure(id); e != "" {
			t.Errorf("%s failed: %q", id, e)
		}
	}
}

// A peer whose address, password or state changed between drains does not get the old
// connection: it is closed, and the new settings get a new one.
func TestDrainPeerChangedDropsConnection(t *testing.T) {
	t.Run("address", func(t *testing.T) {
		a, b := &feedPeer{}, &feedPeer{}
		a.start(t)
		b.start(t)
		q := newFakeQueue(a.peer(1), 2*time.Minute)
		f := newTestFeeder(q)
		t.Cleanup(func() { closeFeeder(f) })
		enqueue(t, q, "move-a", 2)
		f.drain(context.Background())

		q.peer = b.peer(1) // same peer ID, new port
		enqueue(t, q, "move-b", 2)
		f.drain(context.Background())
		if ca, _, ga := a.stats(); ca != 1 || ga != 2 {
			t.Errorf("old address: %d connections, %d articles; want 1, 2", ca, ga)
		}
		if cb, _, gb := b.stats(); cb != 1 || gb != 2 {
			t.Errorf("new address: %d connections, %d articles; want 1, 2", cb, gb)
		}
		waitFor(t, 3*time.Second, "the old connection closed", func() bool {
			_, open, _ := a.stats()
			return open == 0
		})
	})
	t.Run("password", func(t *testing.T) {
		p := &feedPeer{}
		p.start(t)
		peer := p.peer(1)
		peer.OutgoingPassword = "old-secret"
		q := newFakeQueue(peer, 2*time.Minute)
		f := newTestFeeder(q)
		t.Cleanup(func() { closeFeeder(f) })
		enqueue(t, q, "pass-a", 2)
		f.drain(context.Background())

		q.peer.OutgoingPassword = "new-secret"
		enqueue(t, q, "pass-b", 2)
		f.drain(context.Background())
		if conns, _, got := p.stats(); conns != 2 || got != 4 {
			t.Errorf("%d connections, %d articles; want 2, 4", conns, got)
		}
		p.mu.Lock()
		logins := append([]string(nil), p.logins...)
		p.mu.Unlock()
		if len(logins) != 2 || logins[0] != "old-secret" || logins[1] != "new-secret" {
			t.Errorf("AUTHINFO PASS %q, want old-secret then new-secret", logins)
		}
		waitFor(t, 3*time.Second, "only the new connection open", func() bool {
			_, open, _ := p.stats()
			return open == 1
		})
	})
	t.Run("disabled", func(t *testing.T) {
		p := &feedPeer{}
		p.start(t)
		q := newFakeQueue(p.peer(1), 2*time.Minute)
		f := newTestFeeder(q)
		t.Cleanup(func() { closeFeeder(f) })
		enqueue(t, q, "off-a", 1)
		f.drain(context.Background())

		q.peer.Enabled = false
		enqueue(t, q, "off-b", 1)
		f.drain(context.Background())
		if conns, _, got := p.stats(); conns != 1 || got != 1 {
			t.Errorf("%d connections, %d articles; want 1, 1", conns, got)
		}
		if d := q.depth(); d != 0 {
			t.Errorf("queue depth %d, want 0 (item for the disabled peer done)", d)
		}
		waitFor(t, 3*time.Second, "the connection to the disabled peer closed", func() bool {
			_, open, _ := p.stats()
			return open == 0
		})
	})
}

// The worker's shutdown closes the parked connection, and nothing of the feeder keeps
// running afterwards.
func TestWorkerShutdownClosesConnections(t *testing.T) {
	p := &feedPeer{}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	f.tick = 20 * time.Millisecond
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	enqueue(t, q, "stop", 4)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { f.RunWorker(ctx); close(stopped) }()
	waitFor(t, 5*time.Second, "every article at the peer", func() bool {
		_, _, got := p.stats()
		return got == 4 && q.depth() == 0
	})
	if _, open, _ := p.stats(); open != 1 {
		t.Errorf("%d connections open while the worker runs, want 1 (parked)", open)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("RunWorker did not return after cancel")
	}
	waitFor(t, 3*time.Second, "no connection open after the worker stopped", func() bool {
		_, open, _ := p.stats()
		return open == 0
	})
	if c := p.count("QUIT"); c != 1 { // read by the peer before its 205, which quit awaits
		t.Errorf("QUIT sent %d times, want 1", c)
	}
	// A drain after the shutdown (an Offer racing it) does nothing; the item stays queued.
	enqueue(t, q, "late", 1)
	f.drain(context.Background())
	if conns, _, _ := p.stats(); conns != 1 || q.depth() != 1 {
		t.Errorf("after shutdown: %d connections, queue depth %d; want 1, 1", conns, q.depth())
	}
	waitFor(t, 3*time.Second, "the goroutines back to the count before the worker", func() bool {
		return runtime.NumGoroutine() <= base
	})
}

// The tests below are adapted from ous-sol's review fixtures for this branch
// (sol_review_test.go: TestSolReview*), which found the problems they cover.

// scriptConn is a net.Conn that serves a scripted input, then read timeouts; writes
// succeed (from ous-sol's solBufferedConn) unless failAt names the failing write.
type scriptConn struct {
	mu     sync.Mutex
	input  *bytes.Reader
	closed bool
	writes int
	failAt int // the failAt-th Write fails (0: none)
}

func newScriptConn(input string) *scriptConn {
	return &scriptConn{input: bytes.NewReader([]byte(input))}
}

type scriptTimeout struct{}

func (scriptTimeout) Error() string   { return "script read deadline" }
func (scriptTimeout) Timeout() bool   { return true }
func (scriptTimeout) Temporary() bool { return true }

func (c *scriptConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.input.Len() == 0 {
		return 0, scriptTimeout{}
	}
	return c.input.Read(b)
}

func (c *scriptConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.writes++; c.writes == c.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

func (c *scriptConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (*scriptConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*scriptConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*scriptConn) SetDeadline(time.Time) error      { return nil }
func (*scriptConn) SetReadDeadline(time.Time) error  { return nil }
func (*scriptConn) SetWriteDeadline(time.Time) error { return nil }

func scriptSession(c *scriptConn) *session {
	return &session{c: c, nc: nntp.NewConn(c, time.Second), streaming: true}
}

// A closing notice the peer sent right behind its last reply is read ahead into the
// session's reader, not left on the socket; the parked session is not reused.
func TestParkedSessionWithBufferedInputNotAlive(t *testing.T) {
	c := newScriptConn("239 <accepted@test>\r\n400 closing idle connection\r\n")
	s := scriptSession(c)
	if code, _, err := s.nc.ReadReply(); err != nil || code != 239 {
		t.Fatalf("first reply: code %d, err %v", code, err)
	}
	if s.alive() {
		t.Fatal("parked session with a buffered 400 counted as alive")
	}
}

// The worker's shutdown also stops a drain that an Offer started: its connection (here
// waiting for a greeting that never comes) is cut, RunWorker returns only after the drain
// ended, and nothing is reported: the item stays queued.
func TestShutdownCancelsRacingDrain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	p := store.Peer{ID: 1, Name: "silent", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Patterns: "*", Enabled: true}
	q := newFakeQueue(p, 2*time.Minute)
	f := newTestFeeder(q)
	f.timeout = 2 * time.Second
	enqueue(t, q, "racing", 1)
	drained := make(chan struct{})
	go func() { f.drain(context.Background()); close(drained) }() // as Offer does
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain did not connect")
	}
	defer func() { _ = peer.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { f.RunWorker(ctx); close(stopped) }()
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("RunWorker did not return")
	}
	select {
	case <-drained:
	default:
		t.Error("RunWorker returned while the racing drain still ran")
	}
	_ = peer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	var b [1]byte
	if _, err := peer.Read(b[:]); err == nil {
		t.Error("the peer read data, want the connection closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("the racing drain's connection stayed open after RunWorker returned")
	}
	if d, e := q.depth(), q.failure("<racing-0@test>"); d != 1 || e != "" {
		t.Errorf("queue depth %d, failure %q; want 1 and none (nothing reported after shutdown)", d, e)
	}
	for _, l := range q.lateResults() {
		t.Error(l)
	}
}

// An idle timer whose callback runs late, after a drain took the session and parked it
// again, leaves the new parking alone.
func TestIdleTimerLateCallbackKeepsNewParking(t *testing.T) {
	p := &feedPeer{}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	t.Cleanup(func() { closeFeeder(f) })
	enqueue(t, q, "aba-a", 1)
	f.drain(context.Background())

	f.connMu.Lock()
	s := f.idle[1]
	f.connMu.Unlock()
	if s == nil {
		t.Fatal("no parked session after the drain")
	}
	gen := s.parked
	if f.takeSession(1, f.sessionKey(q.peer)) != s {
		t.Fatal("the parked session was not taken")
	}
	f.park(1, s)
	f.expire(1, s, gen) // the first parking's timer, late
	f.connMu.Lock()
	still := f.idle[1] == s
	f.connMu.Unlock()
	if !still {
		t.Fatal("a late timer of the earlier parking expired the new one")
	}
	enqueue(t, q, "aba-b", 1)
	f.drain(context.Background())
	if conns, _, got := p.stats(); conns != 1 || got != 2 {
		t.Errorf("%d connections, %d articles; want 1, 2", conns, got)
	}
}

// RFC 4644 sections 2.4.1 and 2.5.1: 238/431/438 and 239/439 name the offered message-id.
// A reply without it cannot be matched and puts the connection out of step: no result for
// the job, which is left for the caller to fail.
func TestStreamingReplyRequiresID(t *testing.T) {
	const id = "<missing-id@test>"
	for _, tc := range []struct{ name, replies string }{
		{"CHECK238", "238\r\n239 " + id + "\r\n"},
		{"CHECK438", "438 already have it\r\n"},
		{"CHECK431", "431 later\r\n"},
		{"TAKETHIS239", "238 " + id + "\r\n239 transferred\r\n"},
		{"TAKETHIS439", "238 " + id + "\r\n439 rejected\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scriptSession(newScriptConn(tc.replies))
			called := 0
			j := &job{msgid: id, wire: testWire(id), done: func(bool, error) { called++ }}
			left, _, err := s.stream(1, once(j))
			if err == nil || !strings.Contains(err.Error(), "out of step") || called != 0 || len(left) != 1 || left[0] != j {
				t.Fatalf("reply without id: err %v, done %d times, left %d; want out of step, 0, the job", err, called, len(left))
			}
		})
	}
	// 500/501 to CHECK carries no id and still means IHAVE, whatever its text says, even
	// text that starts with "<" (ous-sol's review).
	for _, reply := range []string{"500 what?", "500 <CHECK> unsupported command", "501 <argument> not understood"} {
		t.Run(reply, func(t *testing.T) {
			s := scriptSession(newScriptConn(reply + "\r\n335 send it\r\n235 thanks\r\n"))
			var accepted bool
			j := &job{msgid: id, wire: testWire(id), done: func(a bool, _ error) { accepted = a }}
			if left, _, err := s.stream(1, once(j)); err != nil || len(left) != 0 || !accepted || s.streaming {
				t.Fatalf("err %v, left %d, accepted %v, streaming %v; want the IHAVE fallback", err, len(left), accepted, s.streaming)
			}
		})
	}
}

// A CHECK the peer does not know (500) moves the connection to IHAVE for good: the next
// drain on the parked connection sends no CHECK.
func TestCheckFallbackPersistsAcrossDrains(t *testing.T) {
	forWindows(t, func(t *testing.T, window int) {
		p := &feedPeer{check: map[string]string{"<fallback-0@test>": "500 CHECK unsupported"}}
		p.start(t)
		q := newFakeQueue(p.peer(1), 2*time.Minute)
		f := newTestFeeder(q)
		f.window = window
		t.Cleanup(func() { closeFeeder(f) })
		enqueue(t, q, "fallback", 3)
		f.drain(context.Background())
		checks := p.count("CHECK")
		enqueue(t, q, "fallback-later", 2)
		f.drain(context.Background())
		if p.count("CHECK") != checks || p.count("IHAVE") < 3 || q.depth() != 0 {
			t.Fatalf("CHECK %d (before the second drain %d), IHAVE %d, queue depth %d; want no new CHECK, at least 3 IHAVE, 0",
				p.count("CHECK"), checks, p.count("IHAVE"), q.depth())
		}
		if conns, _, got := p.stats(); conns != 1 || got != 5 {
			t.Fatalf("%d connections, %d articles; want 1, 5", conns, got)
		}
	})
}

// A failed write stops sending, but a 239 that came before it still completes its job.
func TestWriteFailureKeepsPriorAcknowledgement(t *testing.T) {
	const a, b = "<write-a@test>", "<write-b@test>"
	c := newScriptConn("238 " + a + "\r\n238 " + b + "\r\n239 " + a + "\r\n")
	c.failAt = 6 // CHECK a, CHECK b, TAKETHIS a, its article, TAKETHIS b, its article
	s := scriptSession(c)
	var done []string
	newJob := func(id string) *job {
		return &job{msgid: id, wire: testWire(id), done: func(accepted bool, err error) {
			if !accepted || err != nil {
				t.Errorf("%s: accepted %v, err %v; want accepted", id, accepted, err)
			}
			done = append(done, id)
		}}
	}
	ja, jb := newJob(a), newJob(b)
	jobs := []*job{ja, jb}
	next := func() *job {
		if len(jobs) == 0 {
			return nil
		}
		j := jobs[0]
		jobs = jobs[1:]
		return j
	}
	left, _, err := s.stream(2, next)
	if err == nil || len(done) != 1 || done[0] != a || len(left) != 1 || left[0] != jb {
		t.Fatalf("err %v, done %v, left %d; want an error, [%s], the job of %s", err, done, len(left), a, b)
	}
}
