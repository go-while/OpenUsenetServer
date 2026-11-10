package peerauth

import (
	"net"
	"strings"

	"openusenet/internal/inbound"
	"openusenet/internal/store"
)

// MatchPeer finds the enabled peer matching remoteAddr.
func MatchPeer(peers []store.Peer, remoteAddr string) *store.Peer {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	for i := range peers {
		p := peers[i]
		if !p.Enabled {
			continue
		}
		p = p.Normalize()
		for _, rule := range []string{p.IncomingHost, p.Host} {
			if rule == "" {
				continue
			}
			if inbound.MatchHost(remoteAddr, rule) || strings.EqualFold(rule, host) {
				return &peers[i]
			}
		}
	}
	return nil
}

// VerifyFeedAuth checks INN-style feeder password for the matched peer.
func VerifyFeedAuth(peer *store.Peer, password string, require bool) bool {
	if peer == nil {
		return false
	}
	want := strings.TrimSpace(peer.IncomingPassword)
	if want == "" {
		return !require
	}
	return password == want
}
