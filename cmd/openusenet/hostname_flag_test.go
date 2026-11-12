package main

import (
	"testing"

	"openusenet/internal/config"
)

// --hostname used to compare the pathhost with the hostname after overwriting it, so a
// pathhost defaulted from the old hostname (config.Load fills it in) kept the old name, and
// the O-08 check refused to start although the operator had set a real name. Found by ous-sol.
func TestApplyHostnameFlag(t *testing.T) {
	for _, tc := range []struct{ host, path, flag, wantPath string }{
		{"news.example.org", "news.example.org", "news.real-site.org", "news.real-site.org"},   // defaulted
		{"news.example.org", "", "news.real-site.org", "news.real-site.org"},                   // empty
		{"news.example.org", "path.real-site.org", "news.real-site.org", "path.real-site.org"}, // set on purpose
	} {
		cfg := config.Config{Server: config.Server{Hostname: tc.host, Pathhost: tc.path}}
		applyHostnameFlag(&cfg, tc.flag)
		if cfg.Server.Hostname != tc.flag || cfg.Server.Pathhost != tc.wantPath {
			t.Errorf("host %q path %q + --hostname %q: got host %q path %q, want path %q",
				tc.host, tc.path, tc.flag, cfg.Server.Hostname, cfg.Server.Pathhost, tc.wantPath)
		}
	}
}

// config.PathIdentityProblems compares the two names case-insensitively, so --hostname must too:
// with hostname News.Example.org and pathhost news.example.org the pathhost only followed the
// hostname, and it has to follow the flag (a trailing dot is the same name).
func TestApplyHostnameFlagCaseInsensitive(t *testing.T) {
	for _, tc := range []struct{ host, path string }{
		{"News.Example.org", "news.example.org"},
		{"news.example.org", "NEWS.EXAMPLE.ORG"},
		{"news.example.org.", "news.example.org"},
		{"news.example.org", "news.example.org."},
	} {
		cfg := config.Config{Server: config.Server{Hostname: tc.host, Pathhost: tc.path}}
		applyHostnameFlag(&cfg, "news.real-site.org")
		if cfg.Server.Pathhost != "news.real-site.org" {
			t.Errorf("host %q path %q: pathhost %q did not follow --hostname", tc.host, tc.path, cfg.Server.Pathhost)
		}
	}
	cfg := config.Config{Server: config.Server{Hostname: "news.example.org", Pathhost: "path.example.org"}}
	applyHostnameFlag(&cfg, "news.real-site.org")
	if cfg.Server.Pathhost != "path.example.org" {
		t.Errorf("a different pathhost must stay, got %q", cfg.Server.Pathhost)
	}
}
