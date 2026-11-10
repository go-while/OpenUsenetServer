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

// fakePeer is a streaming transit peer on 127.0.0.1 that counts open connections and offers.
type fakePeer struct {
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

func startPeer(t *testing.T, greetDelay time.Duration, trickle bool) *fakePeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePeer{ln: ln, greetDelay: greetDelay, trickle: trickle,
		offers: map[string]int{}, got: map[string]bool{}, closing: make(chan struct{})}
	go p.accept()
	t.Cleanup(func() {
		close(p.closing)
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *fakePeer) storePeer() store.Peer {
	addr := p.ln.Addr().(*net.TCPAddr)
	return store.Peer{ID: 1, Name: "fakepeer", Host: "127.0.0.1", Port: addr.Port, Patterns: "*", Enabled: true}
}

func (p *fakePeer) accept() {
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
			defer func() {
				_ = c.Close()
				p.mu.Lock()
				p.open--
				p.mu.Unlock()
			}()
			p.serve(c)
		}()
	}
}

func (p *fakePeer) serve(c net.Conn) {
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
			w("205 bye")
			return
		default:
			w("500 what?")
		}
	}
}

func (p *fakePeer) snapshot() (maxOpen, conns int, offers map[string]int, got int) {
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
// delivery running past the claim lease: the delivery is cut and the item failed in time.
func TestDeliveryCutAtLease(t *testing.T) {
	const lease = 500 * time.Millisecond
	p := startPeer(t, 0, true)
	q := newFakeQueue(p.storePeer(), lease)
	f := newTestFeeder(q)
	f.lease, f.timeout, f.tick = lease, 200*time.Millisecond, time.Hour

	const id = "<trickle-1@test>"
	_ = q.EnqueueFeed(context.Background(), 1, id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.RunWorker(ctx)

	waitFor(t, 3*time.Second, "the delivery to fail", func() bool { return q.failure(id) != "" })
	for _, l := range q.lateResults() {
		t.Error(l)
	}
}
