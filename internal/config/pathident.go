package config

import (
	"fmt"
	"net"
	"strings"
)

// UnusablePathIdentity reports why name cannot identify this site to peers,
// or "" when it can. RFC 5537 section 3.1.3 and RFC 5536 section 3.1.5: the
// path identity must identify the site uniquely, normally by its FQDN. A name
// that every copy of the example config shares, or that only means something
// on the local machine, breaks loop detection between servers that all use it.
//
// Names under the RFC 2606 ".test" TLD are allowed on purpose: they are chosen
// for test setups, which peer with each other (the tests use them too). So are
// single-label names such as "news-a".
func UnusablePathIdentity(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".")
	switch {
	case n == "":
		return "it is empty"
	case net.ParseIP(strings.Trim(n, "[]")) != nil:
		return "it is an IP address, not a name"
	case n == "localhost" || strings.HasSuffix(n, ".localhost"):
		return "localhost names mean the local machine on every host (RFC 6761)"
	case strings.HasSuffix(n, ".local"):
		return "\".local\" is the multicast DNS link-local domain (RFC 6762)"
	case n == "invalid" || strings.HasSuffix(n, ".invalid"):
		return "\".invalid\" is reserved as never valid (RFC 2606)"
	case n == "example" || strings.HasSuffix(n, ".example"),
		hasDomain(n, "example.org"), hasDomain(n, "example.com"), hasDomain(n, "example.net"):
		return "example names are reserved for documentation (RFC 2606)"
	}
	return ""
}

func hasDomain(name, domain string) bool {
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// PathIdentityProblem is one server name that cannot identify this site to peers.
type PathIdentityProblem struct {
	Setting string // config key(s), e.g. "server.pathhost"
	Env     string // environment variable(s) that override it
	Value   string
	Reason  string
}

func (p PathIdentityProblem) String() string {
	return fmt.Sprintf("%s %q (env %s): %s", p.Setting, p.Value, p.Env, p.Reason)
}

// PathIdentityProblems checks server.hostname (Message-ID and Injection-Info
// domain) and server.pathhost (the Path token, which defaults to the hostname).
// It returns nil when both can identify this site to peers.
func (c Config) PathIdentityProblems() []PathIdentityProblem {
	host := c.Server.Hostname
	path := c.Server.Pathhost
	if strings.TrimSpace(path) == "" {
		path = host
	}
	if strings.EqualFold(strings.TrimSpace(path), strings.TrimSpace(host)) {
		if r := UnusablePathIdentity(host); r != "" {
			return []PathIdentityProblem{{
				Setting: "server.hostname/server.pathhost",
				Env:     "OPENUSENET_HOSTNAME/OPENUSENET_PATHHOST",
				Value:   host,
				Reason:  r,
			}}
		}
		return nil
	}
	var out []PathIdentityProblem
	if r := UnusablePathIdentity(path); r != "" {
		out = append(out, PathIdentityProblem{Setting: "server.pathhost", Env: "OPENUSENET_PATHHOST", Value: path, Reason: r})
	}
	if r := UnusablePathIdentity(host); r != "" {
		out = append(out, PathIdentityProblem{Setting: "server.hostname", Env: "OPENUSENET_HOSTNAME", Value: host, Reason: r})
	}
	return out
}
