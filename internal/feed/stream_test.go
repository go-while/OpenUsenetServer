package feed

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"openusenet/internal/config"
	"openusenet/internal/store"
)

// fakePeer is a minimal NNTP transit peer for deliverArticle tests. It
// records every command line it receives (article bodies excluded).
type fakePeer struct {
	capsReply  []string // full CAPABILITIES reply (status line + body); nil = "500"
	modeStream string   // reply line to MODE STREAM

	mu   sync.Mutex
	cmds []string
}

func (fp *fakePeer) commands() []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return append([]string(nil), fp.cmds...)
}

func (fp *fakePeer) start(t *testing.T) store.Peer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		fp.serve(c)
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return store.Peer{Name: "fake", Host: "127.0.0.1", Port: addr.Port}
}

func (fp *fakePeer) serve(c net.Conn) {
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	send := func(lines ...string) {
		for _, l := range lines {
			_, _ = w.WriteString(l + "\r\n")
		}
		_ = w.Flush()
	}
	readLine := func() (string, bool) {
		l, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(l, "\r\n"), true
	}
	readDot := func() bool {
		for {
			l, ok := readLine()
			if !ok {
				return false
			}
			if l == "." {
				return true
			}
		}
	}
	send("200 fake ready")
	for {
		line, ok := readLine()
		if !ok {
			return
		}
		fp.mu.Lock()
		fp.cmds = append(fp.cmds, line)
		fp.mu.Unlock()
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		switch {
		case verb == "CAPABILITIES":
			if fp.capsReply == nil {
				send("500 Syntax error or bad command")
			} else {
				send(fp.capsReply...)
			}
		case strings.EqualFold(line, "MODE STREAM"):
			send(fp.modeStream)
		case verb == "CHECK":
			send("238 " + strings.Fields(line)[1])
		case verb == "TAKETHIS":
			if !readDot() {
				return
			}
			send("239 " + strings.Fields(line)[1])
		case verb == "IHAVE":
			send("335 send it")
			if !readDot() {
				return
			}
			send("235 thanks")
		case verb == "QUIT":
			send("205 bye")
			return
		default:
			send("500 what?")
		}
	}
}

func hasVerb(cmds []string, verb string) bool {
	for _, c := range cmds {
		if strings.HasPrefix(strings.ToUpper(c), verb) {
			return true
		}
	}
	return false
}

const testMsgID = "<o18@test.invalid>"

func deliverTo(t *testing.T, fp *fakePeer) []string {
	t.Helper()
	p := fp.start(t)
	f := New(config.Config{}, nil, nil, nil)
	f.timeout = 5 * time.Second
	wire := []byte("Message-ID: " + testMsgID + "\r\nNewsgroups: test\r\n\r\nbody\r\n")
	accepted, err := f.deliverArticle(p, testMsgID, wire)
	if err != nil {
		t.Fatalf("deliverArticle: %v (commands %q)", err, fp.commands())
	}
	if !accepted {
		t.Fatalf("article not accepted (commands %q)", fp.commands())
	}
	return fp.commands()
}

// Diablo-style peer: no CAPABILITIES, but MODE STREAM answers 203.
func TestDeliverModeStreamFallbackStreams(t *testing.T) {
	cmds := deliverTo(t, &fakePeer{modeStream: "203 StreamOK."})
	if !hasVerb(cmds, "MODE STREAM") {
		t.Fatalf("MODE STREAM not sent: %q", cmds)
	}
	if !hasVerb(cmds, "CHECK ") || !hasVerb(cmds, "TAKETHIS ") {
		t.Fatalf("expected CHECK/TAKETHIS: %q", cmds)
	}
	if hasVerb(cmds, "IHAVE ") {
		t.Fatalf("unexpected IHAVE: %q", cmds)
	}
}

// Peer with neither CAPABILITIES nor MODE STREAM: IHAVE on the same connection.
func TestDeliverModeStreamRefusedUsesIHave(t *testing.T) {
	cmds := deliverTo(t, &fakePeer{modeStream: "501 command not supported"})
	if !hasVerb(cmds, "MODE STREAM") {
		t.Fatalf("MODE STREAM not sent: %q", cmds)
	}
	if !hasVerb(cmds, "IHAVE ") {
		t.Fatalf("expected IHAVE: %q", cmds)
	}
	if hasVerb(cmds, "CHECK ") || hasVerb(cmds, "TAKETHIS ") {
		t.Fatalf("unexpected streaming: %q", cmds)
	}
}

// RFC 4644 peer advertising STREAMING: no MODE STREAM needed.
func TestDeliverCapabilitiesStreamingNoModeStream(t *testing.T) {
	cmds := deliverTo(t, &fakePeer{
		capsReply:  []string{"101 Capability list:", "VERSION 2", "IHAVE", "STREAMING", "."},
		modeStream: "203 StreamOK.",
	})
	if hasVerb(cmds, "MODE STREAM") {
		t.Fatalf("MODE STREAM sent although STREAMING advertised: %q", cmds)
	}
	if !hasVerb(cmds, "CHECK ") || !hasVerb(cmds, "TAKETHIS ") {
		t.Fatalf("expected CHECK/TAKETHIS: %q", cmds)
	}
	if hasVerb(cmds, "IHAVE ") {
		t.Fatalf("unexpected IHAVE: %q", cmds)
	}
}
