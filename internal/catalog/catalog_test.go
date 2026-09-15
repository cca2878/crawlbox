package catalog

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestRevisionsPagination covers paging in SQL. An unbounded listing grows with
// accumulated history, so each request must cost the page rather than the
// whole source.
func TestRevisionsPagination(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	if e = s.Register(ctx, map[string]string{"one": "fixture"}); e != nil {
		t.Fatal(e)
	}
	base := time.Now().UTC()
	for i := range 5 {
		rev := model.Revision{ID: fmt.Sprintf("rev%d", i), Source: "one", CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Snapshot: fmt.Sprintf("snap%d", i), State: json.RawMessage("null"), Metadata: json.RawMessage("null")}
		if i > 0 {
			rev.Parent = fmt.Sprintf("rev%d", i-1)
		}
		if e = s.Commit(ctx, rev); e != nil {
			t.Fatal(e)
		}
	}
	// Newest first, and the total reports the whole source rather than the page.
	first, total, e := s.RevisionsPage(ctx, "one", 2, 0)
	if e != nil {
		t.Fatal(e)
	}
	if total != 5 || len(first) != 2 || first[0].ID != "rev4" || first[1].ID != "rev3" {
		t.Fatalf("first page %+v total=%d", ids(first), total)
	}
	next, _, e := s.RevisionsPage(ctx, "one", 2, 2)
	if e != nil {
		t.Fatal(e)
	}
	if len(next) != 2 || next[0].ID != "rev2" {
		t.Fatalf("second page %v", ids(next))
	}
	past, _, e := s.RevisionsPage(ctx, "one", 2, 5)
	if e != nil {
		t.Fatal(e)
	}
	if len(past) != 0 {
		t.Fatalf("offset past the end must be empty: %v", ids(past))
	}
	none, total, e := s.RevisionsPage(ctx, "absent", 10, 0)
	if e != nil || total != 0 || len(none) != 0 {
		t.Fatalf("unknown source %v %d %v", ids(none), total, e)
	}
}

func ids(list []model.Revision) []string {
	out := []string{}
	for _, r := range list {
		out = append(out, r.ID)
	}
	return out
}
