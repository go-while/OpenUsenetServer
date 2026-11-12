package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"openusenet/internal/config"
	"openusenet/internal/store"
)

// O-08 follow-up: the startup check runs once. With a placeholder hostname the server starts
// (no peers, warning only), and an admin could then add an enabled peer through the portal; the
// feeder would send articles with the placeholder in Path until the next restart.

func pathIdentPeersPortal(t *testing.T, host string) (http.Handler, *http.Cookie, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	cfg := config.Defaults()
	cfg.Server.Hostname = host
	cfg.Server.Pathhost = host
	h := New(cfg, st, nil, nil, nil, nil, nil).Handler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(`{"username":"admin","password":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || len(rr.Result().Cookies()) == 0 {
		t.Fatalf("setup %d %s", rr.Code, rr.Body.String())
	}
	return h, rr.Result().Cookies()[0], st
}

func pathIdentPeersDo(h http.Handler, c *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	h.ServeHTTP(rr, req)
	return rr
}

func pathIdentPeersCount(t *testing.T, st *store.Memory) int {
	t.Helper()
	n, err := st.CountPeers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

const pathIdentPeersImportBody = `{"text":"peer other {\n ip-name: other.real-site.org\n port-number: 119\n}\n","file_type":"innfeed","apply":true}`

func TestPeerAPIRefusesEnabledPeerWithPlaceholderIdentity(t *testing.T) {
	h, c, st := pathIdentPeersPortal(t, "news.example.org")

	rr := pathIdentPeersDo(h, c, http.MethodPost, "/api/peers", `{"host":"peer.real-site.org","port":119,"enabled":true}`)
	if rr.Code < 400 || rr.Code > 499 {
		t.Fatalf("POST enabled peer: status %d %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"server.hostname", "OPENUSENET_HOSTNAME", "news.example.org"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("error should name %q: %s", want, rr.Body.String())
		}
	}
	if n := pathIdentPeersCount(t, st); n != 0 {
		t.Fatalf("peer created despite the refusal: %d", n)
	}

	// A disabled peer feeds nothing, so it may be prepared.
	rr = pathIdentPeersDo(h, c, http.MethodPost, "/api/peers", `{"host":"peer.real-site.org","port":119,"enabled":false}`)
	if rr.Code != 200 {
		t.Fatalf("POST disabled peer: %d %s", rr.Code, rr.Body.String())
	}
	created := pathIdentPeersCount(t, st)
	if created != 1 {
		t.Fatalf("peers %d", created)
	}
	list, _ := st.ListPeers(context.Background())
	id := list[0].ID

	// Enabling it through PATCH is refused too.
	body := `{"id":` + strconv.FormatInt(id, 10) + `,"host":"peer.real-site.org","port":119,"enabled":true}`
	rr = pathIdentPeersDo(h, c, http.MethodPatch, "/api/peers", body)
	if rr.Code < 400 || rr.Code > 499 {
		t.Fatalf("PATCH enable: status %d %s", rr.Code, rr.Body.String())
	}
	if en, _ := st.ListEnabledPeers(context.Background()); len(en) != 0 {
		t.Fatalf("peer enabled despite the refusal: %v", en)
	}
}

func TestPeerAPIImportINNRefusesEnabledPeerWithPlaceholderIdentity(t *testing.T) {
	h, c, st := pathIdentPeersPortal(t, "news.localhost")
	rr := pathIdentPeersDo(h, c, http.MethodPost, "/api/peers/import-inn", pathIdentPeersImportBody)
	if rr.Code < 400 || rr.Code > 499 {
		t.Fatalf("import apply: status %d %s", rr.Code, rr.Body.String())
	}
	if n := pathIdentPeersCount(t, st); n != 0 {
		t.Fatalf("peer created despite the refusal: %d", n)
	}
}

func TestPeerAPIAllowsEnabledPeerWithRealIdentity(t *testing.T) {
	h, c, st := pathIdentPeersPortal(t, "news.real-site.org")
	rr := pathIdentPeersDo(h, c, http.MethodPost, "/api/peers", `{"host":"peer.real-site.org","port":119,"enabled":true}`)
	if rr.Code != 200 {
		t.Fatalf("POST enabled peer: %d %s", rr.Code, rr.Body.String())
	}
	if en, _ := st.ListEnabledPeers(context.Background()); len(en) != 1 {
		t.Fatalf("enabled peers %d", len(en))
	}
	rr = pathIdentPeersDo(h, c, http.MethodPost, "/api/peers/import-inn", pathIdentPeersImportBody)
	if rr.Code != 200 {
		t.Fatalf("import apply: %d %s", rr.Code, rr.Body.String())
	}
	if n := pathIdentPeersCount(t, st); n != 2 {
		t.Fatalf("peers %d", n)
	}
}
