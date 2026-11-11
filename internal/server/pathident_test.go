package server_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"openusenet/internal/config"
	"openusenet/internal/server"
	"openusenet/internal/store"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func pathIdentCfg(host string) config.Config {
	cfg := config.Defaults()
	cfg.Server.Hostname = host
	cfg.Server.Pathhost = host
	cfg.Listen.NNTP = "127.0.0.1:0"
	cfg.Listen.HTTP = "-"
	return cfg
}

// startOrFail runs ListenAndServe and returns its error, or nil once it listens.
func startOrFail(t *testing.T, cfg config.Config, st store.Store, lg *log.Logger) (*server.Server, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := server.New(cfg, st, nil, lg)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("ListenAndServe returned nil before listening")
			}
			return srv, err
		default:
		}
		if srv.Addr() != nil {
			return srv, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listen timeout")
	return nil, nil
}

// O-08: with peers configured, a placeholder path identity refuses to start.
func TestPathIdentityRefusedWithYAMLPeer(t *testing.T) {
	cfg := pathIdentCfg("news.localhost")
	cfg.Peers = []config.Peer{{Host: "127.0.0.1", Port: 1}}
	st := store.NewMemory()
	_, err := startOrFail(t, cfg, st, log.New(&syncBuf{}, "", 0))
	if err == nil {
		t.Fatal("started with news.localhost and a YAML peer")
	}
	for _, want := range []string{"news.localhost", "server.pathhost", "OPENUSENET_PATHHOST", "OPENUSENET_HOSTNAME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if n, _ := st.CountPeers(context.Background()); n != 0 {
		t.Errorf("refused start still seeded %d peers", n)
	}
}

func TestPathIdentityRefusedWithStorePeer(t *testing.T) {
	cfg := pathIdentCfg("news.example.org")
	st := store.NewMemory()
	if _, err := st.CreatePeer(context.Background(), store.Peer{Host: "peer.real-site.org", Port: 119, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := startOrFail(t, cfg, st, log.New(&syncBuf{}, "", 0)); err == nil {
		t.Fatal("started with news.example.org and an enabled peer")
	}
}

// A bad pathhost is caught even when the hostname is fine.
func TestPathIdentityRefusedPathhostOnly(t *testing.T) {
	cfg := pathIdentCfg("news.real-site.org")
	cfg.Server.Pathhost = "192.0.2.1"
	cfg.Peers = []config.Peer{{Host: "127.0.0.1", Port: 1}}
	_, err := startOrFail(t, cfg, store.NewMemory(), log.New(&syncBuf{}, "", 0))
	if err == nil || !strings.Contains(err.Error(), "server.pathhost") || strings.Contains(err.Error(), "server.hostname") {
		t.Fatalf("want a pathhost-only refusal, got %v", err)
	}
}

// Reader-only use (no peers, or only disabled ones; YAML peers are not seeded
// into a non-empty table) keeps working, with one warning line.
func TestPathIdentityReaderOnlyWarns(t *testing.T) {
	disabled := store.NewMemory()
	if _, err := disabled.CreatePeer(context.Background(), store.Peer{Host: "peer.real-site.org", Port: 119}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		st    store.Store
		peers []config.Peer
	}{
		"no peers":       {store.NewMemory(), nil},
		"disabled peers": {disabled, []config.Peer{{Host: "127.0.0.1", Port: 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := pathIdentCfg("news.localhost")
			cfg.Peers = tc.peers
			buf := &syncBuf{}
			if _, err := startOrFail(t, cfg, tc.st, log.New(buf, "", 0)); err != nil {
				t.Fatalf("reader-only start refused: %v", err)
			}
			if n := strings.Count(buf.String(), "warning: server.hostname/server.pathhost \"news.localhost\""); n != 1 {
				t.Fatalf("want one warning line, got %d in:\n%s", n, buf.String())
			}
		})
	}
}

// A usable name starts with peers and logs no warning.
func TestPathIdentityGoodNameNoWarning(t *testing.T) {
	cfg := pathIdentCfg("news.real-site.org")
	cfg.Peers = []config.Peer{{Host: "127.0.0.1", Port: 1}}
	buf := &syncBuf{}
	if _, err := startOrFail(t, cfg, store.NewMemory(), log.New(buf, "", 0)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "warning:") {
		t.Fatalf("unexpected warning:\n%s", buf.String())
	}
}
