package feed

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"openusenet/internal/config"
	"openusenet/internal/store"
)

// fakeQueue is an in-memory PeerSource and ArticleSource with one peer and a claim lease
// that tests can shorten. It records every CompleteFeed/FailFeed that arrives after the
// lease of its claim ran out (another drain could have claimed the item by then).
type fakeQueue struct {
	mu       sync.Mutex
	lease    time.Duration
	peer     store.Peer
	nextID   int64
	items    []store.FeedQueueItem
	leaseEnd map[int64]time.Time
	late     []string
	failed   map[string]string
}

func newFakeQueue(peer store.Peer, lease time.Duration) *fakeQueue {
	return &fakeQueue{lease: lease, peer: peer, leaseEnd: map[int64]time.Time{}, failed: map[string]string{}}
}

func (q *fakeQueue) ListEnabledPeers(context.Context) ([]store.Peer, error) {
	return []store.Peer{q.peer}, nil
}

func (q *fakeQueue) GetByMsgID(_ context.Context, msgid string) (*store.StoredArticle, error) {
	return &store.StoredArticle{MessageID: msgid, Headers: string(testHead(msgid)), Body: "body line"}, nil
}

func (q *fakeQueue) EnqueueFeed(_ context.Context, peerID int64, msgid string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.items {
		if it.PeerID == peerID && it.MessageID == msgid {
			return nil
		}
	}
	q.nextID++
	q.items = append(q.items, store.FeedQueueItem{ID: q.nextID, PeerID: peerID, MessageID: msgid, NextAttempt: time.Now()})
	return nil
}

func (q *fakeQueue) ClaimFeedDue(_ context.Context, limit int) ([]store.FeedQueueItem, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	var out []store.FeedQueueItem
	for i := range q.items {
		if len(out) >= limit {
			break
		}
		if q.items[i].NextAttempt.After(now) {
			continue
		}
		q.items[i].Attempts++
		q.items[i].NextAttempt = now.Add(q.lease)
		q.leaseEnd[q.items[i].ID] = now.Add(q.lease)
		out = append(out, q.items[i])
	}
	return out, nil
}

// checkLate records a result reported after the item's lease ran out. Callers hold q.mu.
func (q *fakeQueue) checkLate(id int64, what string) {
	if end, ok := q.leaseEnd[id]; ok && time.Now().After(end) {
		q.late = append(q.late, fmt.Sprintf("%s of item %d %s after its lease", what, id, time.Since(end)))
	}
}

func (q *fakeQueue) CompleteFeed(_ context.Context, id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.checkLate(id, "CompleteFeed")
	for i, it := range q.items {
		if it.ID == id {
			q.items = append(q.items[:i], q.items[i+1:]...)
			break
		}
	}
	return nil
}

func (q *fakeQueue) FailFeed(_ context.Context, id int64, errMsg string, retryAfter time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.checkLate(id, "FailFeed")
	for i := range q.items {
		if q.items[i].ID == id {
			q.items[i].LastError = errMsg
			q.items[i].NextAttempt = time.Now().Add(retryAfter)
			q.failed[q.items[i].MessageID] = errMsg
		}
	}
	return nil
}

func (q *fakeQueue) GetPeer(context.Context, int64) (*store.Peer, error) {
	p := q.peer
	return &p, nil
}

func (q *fakeQueue) FeedQueueStats(context.Context) (store.FeedQueueStats, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return store.FeedQueueStats{Depth: len(q.items)}, nil
}

func (q *fakeQueue) GetGroup(context.Context, string) (*store.Group, error) { return nil, nil }

func (q *fakeQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *fakeQueue) lateResults() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.late...)
}

func (q *fakeQueue) failure(msgid string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failed[msgid]
}

func testHead(msgid string) []byte {
	return []byte("Path: origin.example!not-for-mail\r\nFrom: a@example.org\r\nNewsgroups: test.group\r\n" +
		"Subject: drain test\r\nMessage-ID: " + msgid + "\r\nDate: Tue, 10 Nov 2026 12:00:00 +0000")
}

func testWire(msgid string) []byte {
	return append(testHead(msgid), []byte("\r\n\r\nbody line\r\n")...)
}

// drainPeer is a streaming transit peer on 127.0.0.1 that counts open connections and offers.
type drainPeer struct {
	ln         net.Listener
	greetDelay time.Duration // before the greeting
	trickle    bool          // CAPABILITIES never ends: one line every 50 ms

	mu      sync.Mutex
	open    int
	maxOpen int
	conns   int
	offers  map[string]int
	got     map[string]bool
	wg      sync.WaitGroup
	closing chan struct{}
}

func startPeer(t *testing.T, greetDelay time.Duration, trickle bool) *drainPeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &drainPeer{ln: ln, greetDelay: greetDelay, trickle: trickle,
		offers: map[string]int{}, got: map[string]bool{}, closing: make(chan struct{})}
	go p.accept()
	t.Cleanup(func() {
		close(p.closing)
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *drainPeer) storePeer() store.Peer {
	addr := p.ln.Addr().(*net.TCPAddr)
	return store.Peer{ID: 1, Name: "fakepeer", Host: "127.0.0.1", Port: addr.Port, Patterns: "*", Enabled: true}
}

func (p *drainPeer) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.open++
		p.conns++
		p.maxOpen = max(p.maxOpen, p.open)
		p.mu.Unlock()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			// release lowers the open count once: before the 205 reply to QUIT (the client
			// may connect again as soon as it reads it), or when the connection ends.
			var once sync.Once
			release := func() {
				once.Do(func() {
					p.mu.Lock()
					p.open--
					p.mu.Unlock()
				})
			}
			defer func() {
				_ = c.Close()
				release()
			}()
			p.serve(c, release)
		}()
	}
}

func (p *drainPeer) serve(c net.Conn, release func()) {
	go func() { <-p.closing; _ = c.Close() }()
	select {
	case <-time.After(p.greetDelay):
	case <-p.closing:
		return
	}
	w := func(s string) bool { _, err := io.WriteString(c, s+"\r\n"); return err == nil }
	if !w("200 fake peer ready") {
		return
	}
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch strings.ToUpper(verb) {
		case "CAPABILITIES":
			if p.trickle {
				w("101 Capability list:")
				for w("VERSION 2") {
					time.Sleep(50 * time.Millisecond)
				}
				return
			}
			w("101 Capability list:\r\nVERSION 2\r\nIHAVE\r\nSTREAMING\r\n.")
		case "CHECK":
			p.mu.Lock()
			p.offers[arg]++
			p.mu.Unlock()
			w("238 " + arg)
		case "TAKETHIS":
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
			}
			p.mu.Lock()
			p.got[arg] = true
			p.mu.Unlock()
			w("239 " + arg)
		case "QUIT":
			release()
			w("205 bye")
			return
		default:
			w("500 what?")
		}
	}
}

func (p *drainPeer) snapshot() (maxOpen, conns int, offers map[string]int, got int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	offers = make(map[string]int, len(p.offers))
	for k, v := range p.offers {
		offers[k] = v
	}
	return p.maxOpen, p.conns, offers, len(p.got)
}

func newTestFeeder(q *fakeQueue) *Feeder {
	cfg := config.Config{Server: config.Server{Hostname: "news.local", Pathhost: "news.local"}}
	return New(cfg, q, q, log.New(io.Discard, "", 0))
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// O-20: a burst of Offers must not open parallel connections to the peer. Each connection
// is held 30 ms before the greeting so that overlapping drains would show up in maxOpen.
func TestOfferBurstOneConnectionAtATime(t *testing.T) {
	p := startPeer(t, 30*time.Millisecond, false)
	q := newFakeQueue(p.storePeer(), 2*time.Minute)
	f := newTestFeeder(q)

	const n = 20
	for i := range n {
		id := fmt.Sprintf("<burst-%d@test>", i)
		f.Offer(id, "origin.example!not-for-mail", []string{"test.group"}, testWire(id))
	}
	waitFor(t, 10*time.Second, "every article at the peer and an empty queue", func() bool {
		_, _, _, got := p.snapshot()
		return got == n && q.depth() == 0
	})
	maxOpen, conns, offers, _ := p.snapshot()
	if maxOpen > 1 {
		t.Errorf("maxOpen = %d (connections %d), want at most 1 connection at a time", maxOpen, conns)
	}
	for id, c := range offers {
		if c != 1 {
			t.Errorf("%s offered %d times, want 1", id, c)
		}
	}
}

// A burst larger than one claim batch (32) must drain completely from the Offer kicks alone,
// without waiting for worker ticks (there is no worker here): the Offers collapse into one
// pending request, and a full batch has to trigger the next claim.
func TestOfferBurstLargerThanBatch(t *testing.T) {
	p := startPeer(t, 0, false)
	q := newFakeQueue(p.storePeer(), 2*time.Minute)
	f := newTestFeeder(q)

	const n = 100
	for i := range n {
		id := fmt.Sprintf("<bigburst-%d@test>", i)
		f.Offer(id, "origin.example!not-for-mail", []string{"test.group"}, testWire(id))
	}
	waitFor(t, 10*time.Second, "every article at the peer and an empty queue", func() bool {
		_, _, _, got := p.snapshot()
		return got == n && q.depth() == 0
	})
	maxOpen, _, offers, _ := p.snapshot()
	if maxOpen > 1 {
		t.Errorf("maxOpen = %d, want at most 1 connection at a time", maxOpen)
	}
	for id, c := range offers {
		if c != 1 {
			t.Errorf("%s offered %d times, want 1", id, c)
		}
	}
}

// O-21: a batch slower than the claim lease must not be claimed again while it runs, and
// no item may be finished after its lease ran out. 20 queued items at about 40 ms each take
// longer than the 600 ms lease; the worker ticks every 20 ms, and new articles keep arriving
// through Offer (as POSTs do on a live server), each of which kicks a drain.
func TestSlowPeerNoDuplicateOffers(t *testing.T) {
	const lease = 600 * time.Millisecond
	p := startPeer(t, 40*time.Millisecond, false)
	q := newFakeQueue(p.storePeer(), lease)
	f := newTestFeeder(q)
	f.lease, f.timeout, f.tick = lease, 150*time.Millisecond, 20*time.Millisecond

	var ids []string
	for i := range 20 {
		ids = append(ids, fmt.Sprintf("<slow-%d@test>", i))
		_ = q.EnqueueFeed(context.Background(), 1, ids[i])
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.RunWorker(ctx)
	for i := range 10 {
		time.Sleep(100 * time.Millisecond)
		id := fmt.Sprintf("<slow-new-%d@test>", i)
		ids = append(ids, id)
		f.Offer(id, "origin.example!not-for-mail", []string{"test.group"}, testWire(id))
	}

	waitFor(t, 15*time.Second, "every article at the peer and an empty queue", func() bool {
		_, _, _, got := p.snapshot()
		return got == len(ids) && q.depth() == 0
	})
	_, conns, offers, _ := p.snapshot()
	for _, id := range ids {
		if c := offers[id]; c != 1 {
			t.Errorf("%s offered %d times, want 1 (%d connections for %d articles)", id, c, conns, len(ids))
		}
	}
	for _, l := range q.lateResults() {
		t.Error(l)
	}
}

// A peer that never finishes a reply (each line inside the per-step timeout) must not keep a
// delivery running past the claim lease: the delivery is cut before the lease ends, and as
// the claim deadline has passed by then, nothing is reported; the item waits for the next
// claim.
func TestDeliveryCutAtLease(t *testing.T) {
	const lease = 500 * time.Millisecond
	p := startPeer(t, 0, true)
	q := newFakeQueue(p.storePeer(), lease)
	f := newTestFeeder(q)
	f.lease, f.timeout = lease, 200*time.Millisecond

	const id = "<trickle-1@test>"
	_ = q.EnqueueFeed(context.Background(), 1, id)
	start := time.Now()
	done := make(chan struct{})
	go func() { f.drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the delivery was not cut: drain still running after 3s")
	}
	if d := time.Since(start); d >= lease {
		t.Errorf("drain took %s, want less than the %s lease", d, lease)
	}
	for _, l := range q.lateResults() {
		t.Error(l)
	}
	if e := q.failure(id); e != "" {
		t.Errorf("FailFeed after the claim deadline: %q", e)
	}
	if d := q.depth(); d != 1 {
		t.Errorf("queue depth = %d, want 1 (item left for the next claim)", d)
	}
}

// slowLookupQueue answers GetPeer after delay (a stalled store); ctxAware decides whether
// the lookup gives up when its context ends, as a database query does, or not.
type slowLookupQueue struct {
	*fakeQueue
	delay    time.Duration
	ctxAware bool
}

func (s *slowLookupQueue) GetPeer(ctx context.Context, _ int64) (*store.Peer, error) {
	if !s.ctxAware {
		time.Sleep(s.delay)
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return nil, nil
	}
}

// A slow store lookup must not let the drain report a result (CompleteFeed/FailFeed) after
// the claim lease ran out: by then another process may have claimed the item again. The item
// is left for the next claim instead. Adapted from ous-sol's review fixture
// (TestSolReviewStoreLookupRespectsLease): 100 ms lease, 200 ms lookup, peer missing.
func TestStoreLookupRespectsLease(t *testing.T) {
	for _, ctxAware := range []bool{true, false} {
		t.Run(fmt.Sprintf("ctxAware=%v", ctxAware), func(t *testing.T) {
			const lease = 100 * time.Millisecond
			q := newFakeQueue(store.Peer{}, lease)
			s := &slowLookupQueue{fakeQueue: q, delay: 2 * lease, ctxAware: ctxAware}
			f := New(config.Config{}, s, s, log.New(io.Discard, "", 0))
			f.lease, f.timeout = lease, 10*time.Millisecond
			if err := q.EnqueueFeed(context.Background(), 1, "<slow-store@test>"); err != nil {
				t.Fatal(err)
			}
			f.drain(context.Background())
			for _, l := range q.lateResults() {
				t.Error(l)
			}
			if d := q.depth(); d != 1 {
				t.Errorf("queue depth = %d, want 1 (item left for the next claim)", d)
			}
		})
	}
}

// slowResultQueue reports results after delay (a stalled store) and gives up when the
// context ends, as a database query does.
type slowResultQueue struct {
	*fakeQueue
	delay   time.Duration
	failGet bool // GetPeer fails, so the drain reports with FailFeed
}

func (s *slowResultQueue) GetPeer(context.Context, int64) (*store.Peer, error) {
	if s.failGet {
		return nil, fmt.Errorf("lookup failed")
	}
	return nil, nil
}

func (s *slowResultQueue) CompleteFeed(ctx context.Context, id int64) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
		return s.fakeQueue.CompleteFeed(ctx, id)
	}
}

func (s *slowResultQueue) FailFeed(ctx context.Context, id int64, msg string, retry time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
		return s.fakeQueue.FailFeed(ctx, id, msg, retry)
	}
}

// A slow CompleteFeed/FailFeed must give up at the claim deadline, not finish after the
// lease. Adapted from ous-sol's review fixture (TestSolReviewStoreResultsRespectLease).
func TestStoreResultsRespectLease(t *testing.T) {
	for _, failGet := range []bool{false, true} {
		t.Run(fmt.Sprintf("failGet=%v", failGet), func(t *testing.T) {
			const lease = 100 * time.Millisecond
			q := newFakeQueue(store.Peer{}, lease)
			s := &slowResultQueue{fakeQueue: q, delay: 2 * lease, failGet: failGet}
			f := New(config.Config{}, s, s, log.New(io.Discard, "", 0))
			f.lease, f.timeout = lease, 10*time.Millisecond
			if err := q.EnqueueFeed(context.Background(), 1, "<slow-result@test>"); err != nil {
				t.Fatal(err)
			}
			f.drain(context.Background())
			for _, l := range q.lateResults() {
				t.Error(l)
			}
		})
	}
}
