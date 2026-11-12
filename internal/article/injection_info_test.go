package article

import (
	"strings"
	"testing"
)

const injHeaders = "From: a@b.c\r\nNewsgroups: misc.test\r\nSubject: hi\r\n"

func injectPost(t *testing.T, headers, account string) (*Article, error) {
	t.Helper()
	a, err := Parse([]byte(headers + "\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = InjectForPost(a, InjectOpts{Hostname: "news.example", Pathhost: "news.example", PostingAccount: account})
	return a, err
}

// A local post must not carry its own Injection-Info: it is written by the
// injecting server (RFC 5536 3.2.8). INN refuses it the same way.
func TestInjectForPostRefusesInjectionInfo(t *testing.T) {
	for _, h := range []string{
		"Injection-Info: other.example\r\n",
		`Injection-Info: news.example; posting-account="alice"` + "\r\n",
		"injection-info: other.example\r\n",
	} {
		_, err := injectPost(t, injHeaders+h, "bob")
		if err == nil || !strings.Contains(err.Error(), "Injection-Info") {
			t.Errorf("header %q: err %v, want refusal naming Injection-Info", h, err)
		}
	}
	// An empty Injection-Info forges nothing: it is replaced by the server's.
	a, err := injectPost(t, injHeaders+"Injection-Info:\r\n", "bob")
	if err != nil {
		t.Fatalf("empty Injection-Info: %v", err)
	}
	if got := a.Get("Injection-Info"); !strings.Contains(got, `posting-account="bob"`) {
		t.Fatalf("empty Injection-Info not replaced: %q", got)
	}
	if n := strings.Count(strings.ToLower(string(a.Wire())), "\r\ninjection-info:"); n != 1 {
		t.Fatalf("%d Injection-Info lines in %q", n, a.Wire())
	}
}

// The transit path keeps the original injecting server's Injection-Info.
func TestInjectForIHaveKeepsInjectionInfo(t *testing.T) {
	a, err := Parse([]byte(injHeaders + "Message-ID: <x@y.z>\r\nDate: Mon, 01 Jan 2024 00:00:00 +0000\r\nInjection-Info: origin.example; posting-account=\"alice\"\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := InjectForIHave(a, InjectOpts{Hostname: "news.example", Pathhost: "news.example"}); err != nil {
		t.Fatal(err)
	}
	if got, want := a.Get("Injection-Info"), `origin.example; posting-account="alice"`; got != want {
		t.Fatalf("Injection-Info %q, want %q", got, want)
	}
}

func TestInjectForPostPostingAccountSafe(t *testing.T) {
	cases := []struct {
		name, account, want string
	}{
		{"plain", "nc-interop", `news.example; posting-account="nc-interop"`},
		{"escaped", `a"b\c`, `news.example; posting-account="a\"b\\c"`},
		{"empty", "", "news.example"},
		{"newline", "a\r\nb", "news.example"},
		{"nul", "a\x00b", "news.example"},
		{"del", "a\x7fb", "news.example"},
		{"utf8", "J\u00fcrgen", "news.example"},
		{"cjk", "\u7528\u6237", "news.example"},
		{"max length", strings.Repeat("u", 64), `news.example; posting-account="` + strings.Repeat("u", 64) + `"`},
		{"too long", strings.Repeat("u", 65), "news.example"},
		{"very long", strings.Repeat("u", 5000), "news.example"},
	}
	for _, c := range cases {
		a, err := injectPost(t, injHeaders, c.account)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := a.Get("Injection-Info")
		if got != c.want {
			t.Errorf("%s: Injection-Info %q, want %q", c.name, got, c.want)
		}
		for _, line := range strings.Split(string(a.Wire()), "\r\n") {
			if len(line) > 998 {
				t.Errorf("%s: header line of %d octets", c.name, len(line))
			}
		}
	}
	// The worst case with escaping stays well under the line limit.
	a, err := injectPost(t, injHeaders, strings.Repeat(`"`, 64))
	if err != nil {
		t.Fatal(err)
	}
	if n := len("Injection-Info: ") + len(a.Get("Injection-Info")); n > 998 {
		t.Errorf("escaped account makes a %d octet line", n)
	}
}
