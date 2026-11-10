package nntp_test

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"openusenet/internal/auth"
	"openusenet/internal/store"
)

// A refused POST must get exactly one reply; before the fix the server also sent 340
// and then read the client's next commands as the article.
func TestPostRefusedGetsOneReply(t *testing.T) {
	c, st := startTestServer(t)
	ctx := context.Background()
	hash, err := auth.HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, store.User{Username: "reader", PasswordHash: hash, Role: store.RoleUser}); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second)) // hang guard; bcrypt is slow under -race
	r := bufio.NewReader(c)
	readLine(t, r) // greeting

	cmd := func(line, wantPrefix string) {
		t.Helper()
		if _, err := c.Write([]byte(line + "\r\n")); err != nil {
			t.Fatalf("write %q: %v (server out of sync?)", line, err)
		}
		if got := readLine(t, r); !strings.HasPrefix(got, wantPrefix) {
			t.Fatalf("%s: got %q, want %s...", line, got, wantPrefix)
		}
	}

	// Not logged in: 480, then the next command is answered normally.
	cmd("POST", "480 ")
	cmd("DATE", "111 ")

	// Logged in, but the user may not post: 440, then the next command is answered normally.
	cmd("AUTHINFO USER reader", "381 ")
	cmd("AUTHINFO PASS secret", "281 ")
	cmd("POST", "440 ")
	cmd("DATE", "111 ")
}
