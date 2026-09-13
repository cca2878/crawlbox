package catalog

import (
	"context"
	"encoding/json"
	"github.com/cca2878/crawlbox/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenLifecycleAndRetirement(t *testing.T) {
	p := filepath.Join(t.TempDir(), "catalog.db")
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	if e = s.Register(ctx, map[string]string{"one": "fixture", "two": "fixture"}); e != nil {
		t.Fatal(e)
	}
	expiry := time.Now().Add(time.Hour)
	token, secret, e := s.CreateToken(ctx, "reader", []string{"one"}, &expiry)
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.Authenticate(ctx, secret)
	if e != nil || len(got.Sources) != 1 || got.Sources[0] != "one" {
		t.Fatalf("auth %+v %v", got, e)
	}
	var digest, body string
	if e = s.DB.QueryRow("SELECT hex(digest),body FROM tokens WHERE id=?", token.ID).Scan(&digest, &body); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(body, strings.Split(secret, ".")[1]) || len(digest) != 64 {
		t.Fatal("plaintext storage")
	}
	if _, e = s.Authenticate(ctx, secret+"bad"); e == nil {
		t.Fatal("bad token authenticated")
	}
	if e = s.Revoke(ctx, token.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, secret); e == nil {
		t.Fatal("revoked token authenticated")
	}
	if e = s.Register(ctx, map[string]string{"two": "fixture"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Register(ctx, map[string]string{"one": "fixture", "two": "fixture"}); e == nil {
		t.Fatal("retired source reintroduced")
	}
	_ = os.Chmod(p, 0600)
}
func TestExpiryAndRevisionCommit(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	if e = s.Register(ctx, map[string]string{"one": "fixture"}); e != nil {
		t.Fatal(e)
	}
	expires := time.Now().Add(time.Hour)
	tok, secret, e := s.CreateToken(ctx, "x", []string{"one"}, &expires)
	if e != nil {
		t.Fatal(e)
	}
	past := time.Now().Add(-time.Hour)
	tok.Expires = &past
	b, _ := json.Marshal(tok)
	if _, e = s.DB.Exec("UPDATE tokens SET body=? WHERE id=?", string(b), tok.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, secret); e == nil {
		t.Fatal("expired token accepted")
	}
	r := model.Revision{ID: "first", Source: "one", State: json.RawMessage(`{"cursor":1}`), Snapshot: "snap", CreatedAt: time.Now()}
	if e = s.Commit(ctx, r); e != nil {
		t.Fatal(e)
	}
	if e = s.Commit(ctx, r); e != nil {
		t.Fatal(e)
	}
	r.ID = "second"
	if e = s.Commit(ctx, r); e == nil {
		t.Fatal("stale candidate committed")
	}
	r.Parent = "first"
	if e = s.Commit(ctx, r); e != nil {
		t.Fatal(e)
	}
}
