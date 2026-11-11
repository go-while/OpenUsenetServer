package feed

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"openusenet/internal/store"
)

// feedPeer is a transit peer on 127.0.0.1 for the connection tests (O-17). It counts
// connections and commands, and its replies can be set per message-id. Configure the
// fields before start.
type feedPeer struct {
	noStream   bool              // CAPABILITIES 500 and MODE STREAM 501: IHAVE only (O-18 refusal path)
	check      map[string]string // reply line to CHECK <id>; default "238 <id>"
	take       map[string]string // reply line to TAKETHIS <id>; default "239 <id>"
	dropTake   string            // close the connection after reading the article of TAKETHIS <dropTake>, without a reply
	idleClose  time.Duration     // close a connection that sends nothing for this long, after "400 idle"
	dropIdle   time.Duration     // close a connection without a word when a command comes after this long a pause
	firstCheck time.Duration     // before answering the first CHECK of a connection, wait this long and count the lines already sent

	ln      net.Listener
	closing chan struct{}
	wg      sync.WaitGroup

	mu     sync.Mutex
	conns  int            // connections accepted
	open   int            // connections not yet ended
	cmds   map[string]int // first word of every command line, upper-cased
	got    map[string]int // articles received (TAKETHIS answered 239, IHAVE answered 235) per message-id
	logins []string       // AUTHINFO PASS arguments in order
	ahead  []int          // per connection: command lines already sent when its first CHECK was answered
}

func (p *feedPeer) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.ln, p.closing = ln, make(chan struct{})
	p.cmds, p.got = map[string]int{}, map[string]int{}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns++
			p.open++
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
	}()
	t.Cleanup(func() {
		close(p.closing)
		_ = ln.Close()
		p.wg.Wait()
	})
}

func (p *feedPeer) peer(id int64) store.Peer {
	return store.Peer{ID: id, Name: "connpeer", Host: "127.0.0.1", Port: p.ln.Addr().(*net.TCPAddr).Port, Patterns: "*", Enabled: true}
}

func (p *feedPeer) serve(c net.Conn) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-p.closing:
		case <-done:
		}
		_ = c.Close()
	}()
	r := bufio.NewReader(c)
	w := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	readDot := func() bool {
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return false
			}
			if l == ".\r\n" {
				return true
			}
		}
	}
	w("200 conn peer ready")
	checks, last := 0, time.Now()
	for {
		if p.idleClose > 0 {
			_ = c.SetReadDeadline(time.Now().Add(p.idleClose))
		}
		line, err := r.ReadString('\n')
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				w("400 idle for too long")
			}
			return
		}
		if p.dropIdle > 0 && time.Since(last) > p.dropIdle {
			return
		}
		last = time.Now()
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		p.mu.Lock()
		p.cmds[verb]++
		p.mu.Unlock()
		switch verb {
		case "CAPABILITIES":
			if p.noStream {
				w("500 what?")
			} else {
				w("101 Capability list:\r\nVERSION 2\r\nIHAVE\r\nSTREAMING\r\n.")
			}
		case "MODE":
			if p.noStream {
				w("501 not supported")
			} else {
				w("203 StreamOK.")
			}
		case "AUTHINFO":
			kind, val, _ := strings.Cut(arg, " ")
			if strings.EqualFold(kind, "USER") {
				w("381 password please")
			} else {
				p.mu.Lock()
				p.logins = append(p.logins, val)
				p.mu.Unlock()
				w("281 ok")
			}
		case "CHECK":
			if checks++; checks == 1 && p.firstCheck > 0 {
				time.Sleep(p.firstCheck)
				peek, _ := r.Peek(r.Buffered())
				p.mu.Lock()
				p.ahead = append(p.ahead, bytes.Count(peek, []byte("\n")))
				p.mu.Unlock()
			}
			if reply, ok := p.check[arg]; ok {
				w(reply)
			} else {
				w("238 " + arg)
			}
		case "TAKETHIS":
			if !readDot() || arg == p.dropTake {
				return
			}
			reply, ok := p.take[arg]
			if !ok {
				reply = "239 " + arg
			}
			if strings.HasPrefix(reply, "239") {
				p.mu.Lock()
				p.got[arg]++
				p.mu.Unlock()
			}
			w(reply)
		case "IHAVE":
			w("335 send it")
			if !readDot() {
				return
			}
			p.mu.Lock()
			p.got[arg]++
			p.mu.Unlock()
			w("235 thanks")
		case "QUIT":
			w("205 bye")
			return
		default:
			w("500 what?")
		}
	}
}

func (p *feedPeer) count(verb string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmds[verb]
}

func (p *feedPeer) stats() (conns, open, got int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns, p.open, len(p.got)
}

func (p *feedPeer) received(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.got[id]
}

// enqueue puts n articles named <prefix-i@test> for peer 1 into q and returns their ids.
func enqueue(t *testing.T, q *fakeQueue, prefix string, n int) []string {
	t.Helper()
	var ids []string
	for i := range n {
		id := fmt.Sprintf("<%s-%d@test>", prefix, i)
		if err := q.EnqueueFeed(context.Background(), 1, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// O-17: one drain carries all articles for a streaming peer on one connection, with one
// CHECK and one TAKETHIS each (the feeder used to open a connection per article).
func TestDrainOneConnectionPerPeer(t *testing.T) {
	p := &feedPeer{}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	t.Cleanup(func() { closeFeeder(f) })

	const n = 10
	ids := enqueue(t, q, "one-conn", n)
	f.drain(context.Background())

	conns, _, got := p.stats()
	if conns != 1 {
		t.Errorf("%d articles used %d connections, want 1", n, conns)
	}
	if c, tk := p.count("CHECK"), p.count("TAKETHIS"); c != n || tk != n {
		t.Errorf("CHECK %d, TAKETHIS %d, want %d each", c, tk, n)
	}
	if c := p.count("CAPABILITIES"); c != 1 {
		t.Errorf("CAPABILITIES sent %d times, want 1 (once per connection)", c)
	}
	if got != n || q.depth() != 0 {
		t.Errorf("peer got %d articles, queue depth %d; want %d and 0", got, q.depth(), n)
	}
	for _, id := range ids {
		if c := p.received(id); c != 1 {
			t.Errorf("%s received %d times, want 1", id, c)
		}
	}
}

// O-17 with O-18's refusal path: a peer that knows neither CAPABILITIES nor MODE STREAM
// gets all articles with IHAVE on one connection.
func TestDrainIHaveOnlyOneConnection(t *testing.T) {
	p := &feedPeer{noStream: true}
	p.start(t)
	q := newFakeQueue(p.peer(1), 2*time.Minute)
	f := newTestFeeder(q)
	t.Cleanup(func() { closeFeeder(f) })

	const n = 6
	enqueue(t, q, "ihave-conn", n)
	f.drain(context.Background())

	conns, _, got := p.stats()
	if conns != 1 {
		t.Errorf("%d articles used %d connections, want 1", n, conns)
	}
	if ih, ms, c := p.count("IHAVE"), p.count("MODE"), p.count("CHECK"); ih != n || ms != 1 || c != 0 {
		t.Errorf("IHAVE %d, MODE STREAM %d, CHECK %d; want %d, 1, 0", ih, ms, c, n)
	}
	if got != n || q.depth() != 0 {
		t.Errorf("peer got %d articles, queue depth %d; want %d and 0", got, q.depth(), n)
	}
}
