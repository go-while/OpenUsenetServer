package peerauth

import (
	"testing"

	"openusenet/internal/store"
)

func TestPairPasswordSymmetric(t *testing.T) {
	a := PairPassword("news-a", "news-b")
	b := PairPassword("news-b", "news-a")
	if a == "" || a != b {
		t.Fatalf("a=%q b=%q", a, b)
	}
}

func TestFillPasswords(t *testing.T) {
	p := store.Peer{Host: "news-b", IncomingHost: "news-b"}
	FillPasswords("news-a", &p)
	if p.IncomingPassword == "" || p.OutgoingPassword == "" {
		t.Fatal("expected passwords filled")
	}
	if p.IncomingPassword != p.OutgoingPassword {
		t.Fatal("pair password should match both directions")
	}
}

func TestVerifyFeedAuth(t *testing.T) {
	p := &store.Peer{IncomingPassword: "secret"}
	if !VerifyFeedAuth(p, "secret", true) {
		t.Fatal("expected ok")
	}
	if VerifyFeedAuth(p, "wrong", true) {
		t.Fatal("expected fail")
	}
}

func TestUsesPairPassword(t *testing.T) {
	p := store.Peer{Host: "news-b", IncomingHost: "news-b"}
	if UsesPairPassword("news-a", p) {
		t.Fatal("no password set: not the old scheme")
	}
	FillPasswords("news-a", &p)
	if !UsesPairPassword("news-a", p) {
		t.Fatal("passwords filled by the old scheme not detected")
	}
	p.IncomingPassword, p.OutgoingPassword = "random-1", "random-2"
	if UsesPairPassword("news-a", p) {
		t.Fatal("chosen passwords reported as the old scheme")
	}
	p.OutgoingPassword = PairPassword("news-a", "news-b")
	if !UsesPairPassword("news-a", p) {
		t.Fatal("old outgoing password alone not detected")
	}
}
