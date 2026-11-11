package feed

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// closeFeeder closes the connections f keeps open for the next drain.
func closeFeeder(f *Feeder) { f.closeIdle() }

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
func TestDrainMixedStreamingReplies(t *testing.T) {
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
func TestDrainReplyOutOfStep(t *testing.T) {
	p := &feedPeer{check: map[string]string{"<step-1@test>": "238 <other@test>"}}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	t.Cleanup(func() { closeFeeder(f) })
	ids := enqueue(t, q, "step", 3)
	f.drain(context.Background())

	if p.received(ids[0]) != 1 {
		t.Errorf("%s not received", ids[0])
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
func TestDrainTransportErrorMidBatch(t *testing.T) {
	p := &feedPeer{dropTake: "<mid-3@test>"}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
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
	if _, open, _ := p.stats(); open != 0 {
		t.Errorf("%d connections open after the worker stopped, want 0", open)
	}
	if c := p.count("QUIT"); c != 1 {
		t.Errorf("QUIT sent %d times, want 1", c)
	}
	// A drain after the shutdown (an Offer racing it) parks nothing.
	enqueue(t, q, "late", 1)
	f.drain(context.Background())
	waitFor(t, 3*time.Second, "no connection open", func() bool {
		_, open, _ := p.stats()
		return open == 0
	})
	waitFor(t, 3*time.Second, "the goroutines back to the count before the worker", func() bool {
		return runtime.NumGoroutine() <= base
	})
}
