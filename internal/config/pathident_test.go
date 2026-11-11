package config

import (
	"strings"
	"testing"
)

func TestUnusablePathIdentity(t *testing.T) {
	bad := []string{
		"", "  ",
		"localhost", "LOCALHOST", "news.localhost", "news.localhost.",
		"news.local", "box.lan.local",
		"invalid", "news.invalid",
		"example", "news.example",
		"example.org", "news.example.org", "example.com", "a.b.example.com", "example.net", "news.example.net",
		"127.0.0.1", "10.10.11.116", "::1", "[::1]", "2001:db8::1",
	}
	for _, name := range bad {
		if UnusablePathIdentity(name) == "" {
			t.Errorf("%q: accepted, want refused", name)
		}
	}
	good := []string{
		"news.eternal-september.org", "News.Example-Site.org", "news.gwene.org.",
		"news-a", "news-b", "e2e.test", "news.test", // single labels and .test stay usable (test setups)
		"myexample.org", "example.org.uk", "localhost.example-isp.de", "news.locality.net",
		"local", "newslocalhost.de",
	}
	for _, name := range good {
		if r := UnusablePathIdentity(name); r != "" {
			t.Errorf("%q: refused (%s), want accepted", name, r)
		}
	}
}

func TestPathIdentityProblems(t *testing.T) {
	cfg := Defaults()
	if p := cfg.PathIdentityProblems(); len(p) != 1 || p[0].Setting != "server.hostname/server.pathhost" ||
		!strings.Contains(p[0].String(), "OPENUSENET_HOSTNAME") {
		t.Fatalf("defaults: %v", p)
	}
	cfg.Server.Hostname = "news.real-site.org"
	if p := cfg.PathIdentityProblems(); p != nil {
		t.Fatalf("pathhost defaults to a good hostname: %v", p)
	}
	cfg.Server.Pathhost = "news.localhost"
	p := cfg.PathIdentityProblems()
	if len(p) != 1 || p[0].Setting != "server.pathhost" || p[0].Env != "OPENUSENET_PATHHOST" {
		t.Fatalf("bad pathhost: %v", p)
	}
	cfg.Server.Hostname = "news.example.org"
	if p := cfg.PathIdentityProblems(); len(p) != 2 {
		t.Fatalf("both bad and different: %v", p)
	}
}

// The shipped example must not pass for a real path identity.
func TestExampleConfigIsPlaceholder(t *testing.T) {
	t.Setenv("OPENUSENET_HOSTNAME", "")
	t.Setenv("OPENUSENET_PATHHOST", "")
	cfg, err := Load("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.PathIdentityProblems(); len(p) == 0 {
		t.Fatalf("config.example.yml hostname %q / pathhost %q pass as usable", cfg.Server.Hostname, cfg.Server.Pathhost)
	}
}
