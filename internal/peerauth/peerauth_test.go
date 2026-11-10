package peerauth

import (
	"testing"

	"openusenet/internal/store"
)

func TestVerifyFeedAuth(t *testing.T) {
	p := &store.Peer{IncomingPassword: "secret"}
	if !VerifyFeedAuth(p, "secret", true) {
		t.Fatal("expected ok")
	}
	if VerifyFeedAuth(p, "wrong", true) {
		t.Fatal("expected fail")
	}
}
