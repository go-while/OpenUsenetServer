package server_test

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"openusenet/internal/config"
	"openusenet/internal/server"
	"openusenet/internal/store"
)

// watchdogOff keeps the load watchdog out of these tests: on a busy machine it pauses
// intake (441 load average elevated) and the test fails for reasons unrelated to the code.
var watchdogOff = false

func waitAddr(t *testing.T, srv *server.Server) net.Addr {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != nil {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listen timeout")
	return nil
}

func TestPOSTFeedsIHAVE(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stA := store.NewMemory()
	stB := store.NewMemory()
	if err := stA.EnsureGroup(ctx, "local.test", "Local test group", "y"); err != nil {
		t.Fatal(err)
	}
	if err := stB.EnsureGroup(ctx, "local.test", "Local test group", "y"); err != nil {
		t.Fatal(err)
	}

	cfgB := config.Defaults()
	cfgB.Server.Hostname = "news-b"
	cfgB.Server.Pathhost = "news-b"
	cfgB.Listen.NNTP = "127.0.0.1:0"
	cfgB.Listen.HTTP = "-"
	cfgB.Watchdog.Enabled = &watchdogOff
	srvB := server.New(cfgB, stB, nil, nil)
	go func() { _ = srvB.ListenAndServe(ctx) }()
	addrB := waitAddr(t, srvB)
	_, portStr, err := net.SplitHostPort(addrB.String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	cfgA := config.Defaults()
	cfgA.Server.Hostname = "news-a"
	cfgA.Server.Pathhost = "news-a"
	cfgA.Listen.NNTP = "127.0.0.1:0"
	cfgA.Listen.HTTP = "-"
	cfgA.Watchdog.Enabled = &watchdogOff
	cfgA.Peers = []config.Peer{{Host: "127.0.0.1", Port: port}}
	srvA := server.New(cfgA, stA, nil, nil)
	go func() { _ = srvA.ListenAndServe(ctx) }()
	addrA := waitAddr(t, srvA)

	c, err := net.Dial("tcp", addrA.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	greet, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(greet, "200 ") {
		t.Fatalf("greeting %q %v", greet, err)
	}
	if _, err := c.Write([]byte("POST\r\n")); err != nil {
		t.Fatal(err)
	}
	cont, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(cont, "340 ") {
		t.Fatalf("post cont %q %v", cont, err)
	}
	art := "From: tester@example.com\r\n" +
		"Newsgroups: local.test\r\n" +
		"Subject: mesh\r\n" +
		"\r\n" +
		"hello peers\r\n" +
		".\r\n"
	if _, err := c.Write([]byte(art)); err != nil {
		t.Fatal(err)
	}
	posted, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(posted, "240 ") {
		t.Fatalf("posted %q %v", posted, err)
	}
	fields := strings.Fields(posted)
	var msgid string
	for _, f := range fields {
		if strings.HasPrefix(f, "<") {
			msgid = f
			break
		}
	}
	if msgid == "" {
		t.Fatalf("no msgid in %q", posted)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := stB.HasMessageID(ctx, msgid)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			got, err := stB.GetByMsgID(ctx, msgid)
			if err != nil || got == nil {
				t.Fatalf("get %v %v", got, err)
			}
			if !strings.Contains(got.Headers, "Path: news-b!news-a!") {
				t.Fatalf("expected path hop, got %q", got.Headers)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("news-b did not receive %s", msgid)
}

// A peer configured by hostname is admitted through a forward DNS match, but its group
// patterns were only applied when the connecting IP equalled a configured host field as text,
// so a hostname peer could feed any group. Its patterns apply now.
func TestHostnamePeerPatternsApply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := store.NewMemory()
	for _, g := range []string{"local.test", "alt.test"} {
		if err := st.EnsureGroup(ctx, g, "test group", "y"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreatePeer(ctx, store.Peer{Name: "hostpeer", Host: "localhost", Port: 1, Patterns: "local.*", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Server.Hostname = "news-a"
	cfg.Server.Pathhost = "news-a"
	cfg.Listen.NNTP = "127.0.0.1:0"
	cfg.Listen.HTTP = "-"
	closed := false
	cfg.Inbound.Open = &closed // admission by the peer's host only
	off := false
	cfg.Watchdog.Enabled = &off // a busy test host must not refuse the article (436/441)
	srv := server.New(cfg, st, nil, nil)
	go func() { _ = srv.ListenAndServe(ctx) }()
	addr := waitAddr(t, srv)

	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	if greet, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(greet, "200 ") {
		t.Fatalf("greeting %q %v", greet, err)
	}
	ihave := func(msgid, group, want string) {
		t.Helper()
		if _, err := c.Write([]byte("IHAVE " + msgid + "\r\n")); err != nil {
			t.Fatal(err)
		}
		if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "335 ") {
			t.Fatalf("IHAVE %s: %q %v", msgid, line, err)
		}
		art := "Path: hostpeer!not-for-mail\r\nFrom: a@b.example\r\nNewsgroups: " + group +
			"\r\nSubject: s\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) +
			"\r\nMessage-ID: " + msgid + "\r\n\r\nbody\r\n.\r\n"
		if _, err := c.Write([]byte(art)); err != nil {
			t.Fatal(err)
		}
		if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, want) {
			t.Fatalf("article to %s: %q %v, want %s...", group, line, err, want)
		}
	}
	ihave("<wanted@hostpeer.example>", "local.test", "235 ")
	ihave("<unwanted@hostpeer.example>", "alt.test", "437 ")
}
