package posting

import (
	"context"
	"strings"
	"testing"
)

// The web form fields are written into the header block, so a newline in
// Subject can smuggle in an Injection-Info header for another account.
func TestAcceptRefusesInjectionInfo(t *testing.T) {
	st, cfg := moderatedFixture(t)
	ctx := context.Background()
	offers := &offerLog{}
	_, err := Accept(ctx, cfg, st, nil, offers, nil, Input{
		From: "bob@example.org", Newsgroups: "local.test", Body: "body\n",
		Subject: "hi\nInjection-Info: news.test; posting-account=\"alice\"",
	}, 2, "bob")
	if err == nil || !strings.Contains(err.Error(), "Injection-Info") {
		t.Fatalf("err %v, want refusal naming Injection-Info", err)
	}
	if n, _ := st.CountArticles(ctx); n != 0 || offers.n != 0 {
		t.Fatalf("stored or offered: articles=%d offers=%d", n, offers.n)
	}
}
