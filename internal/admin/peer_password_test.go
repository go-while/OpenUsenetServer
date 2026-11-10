package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"openusenet/internal/config"
	"openusenet/internal/store"
)

// The incoming.conf snippet on a peer's page used to carry a password derived from the two
// path hosts (both public in every Path header) and a fixed string, so anyone could compute
// it. It is now random per peer, stored, stable across views, and the peer's outgoing
// password when one is set. A peer still using the old derived value gets a warning.
func TestPeerSnippetPasswordRandomAndStable(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.Hostname = "news.ous-demo.example"
	cfg.Server.Pathhost = "news.ous-demo.example"
	h := New(cfg, store.NewMemory(), nil, nil, nil, nil, nil).Handler()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(`{"username":"admin","password":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	cookie := rr.Result().Cookies()[0]

	call := func(method, url, body string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, url, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, url, rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	passRe := regexp.MustCompile(`(?m)^\s*password:\s+(\S+)$`)
	snippetPass := func(d map[string]any) string {
		t.Helper()
		inc := d["our_side"].(map[string]any)["incoming"].(string)
		m := passRe.FindStringSubmatch(inc)
		if m == nil {
			t.Fatalf("no password in incoming.conf snippet:\n%s", inc)
		}
		return m[1]
	}
	peerID := func(d map[string]any) string {
		return jsonNumber(d["peer"].(map[string]any)["id"]) // JSON numbers decode as float64; 1 marshals as "1"
	}

	a := call(http.MethodPost, "/api/peers", `{"name":"peer","host":"news.peer.example","path_token":"news.peer.example","port":119,"patterns":"*"}`)
	pa := snippetPass(a)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(pa) {
		t.Fatalf("snippet password %q, want 32 hex digits", pa)
	}
	// The old scheme: sha256 of the sorted path hosts and a fixed string.
	sum := sha256.Sum256([]byte("news.ous-demo.example\x00news.peer.example\x00openusenet-peer"))
	if pa == hex.EncodeToString(sum[:8]) {
		t.Fatal("snippet password is still derived from the path hosts")
	}
	if again := snippetPass(call(http.MethodGet, "/api/peers?id="+peerID(a), "")); again != pa {
		t.Fatalf("snippet password changed between views: %q then %q", pa, again)
	}
	b := call(http.MethodPost, "/api/peers", `{"name":"other","host":"news.other.example","port":119,"patterns":"*"}`)
	if pb := snippetPass(b); pb == pa {
		t.Fatal("two peers got the same snippet password")
	}
	c := call(http.MethodPost, "/api/peers", `{"name":"set","host":"news.set.example","port":119,"patterns":"*","outgoing_password":"chosen-by-admin"}`)
	if pc := snippetPass(c); pc != "chosen-by-admin" {
		t.Fatalf("snippet password %q, want the peer's outgoing password", pc)
	}
	for _, d := range []map[string]any{a, b, c} {
		if w, ok := d["password_warning"]; ok {
			t.Fatalf("unexpected password warning for %v: %v", d["peer"], w)
		}
	}

	// A peer whose stored password is still the old derived value gets a warning, on
	// creation and when it is opened for editing.
	old := hex.EncodeToString(sum[:8])
	o := call(http.MethodPost, "/api/peers", `{"name":"old","host":"news.peer.example","path_token":"news.peer.example","port":119,"patterns":"*","outgoing_password":"`+old+`"}`)
	if _, ok := o["password_warning"]; !ok {
		t.Fatal("no warning for a peer that still uses the old derived password")
	}
	if _, ok := call(http.MethodGet, "/api/peers?id="+peerID(o), "")["password_warning"]; !ok {
		t.Fatal("no warning when opening the peer for editing")
	}
}

func jsonNumber(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
