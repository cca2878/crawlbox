package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cca2878/crawlbox/internal/app"
	"github.com/cca2878/crawlbox/internal/catalog"
	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/model"
	rt "github.com/cca2878/crawlbox/internal/runtime"
	"github.com/cca2878/crawlbox/internal/testutil"
	"github.com/cca2878/crawlbox/internal/web"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLifecycleRecoveryAndAuthorization(t *testing.T) {
	wasm := os.Getenv("FIXTURE_WASM")
	if wasm == "" {
		t.Skip("FIXTURE_WASM required")
	}
	k := testutil.Kopia(t)
	b, e := os.ReadFile(wasm)
	if e != nil {
		t.Fatal(e)
	}
	data := t.TempDir()
	cfg := config.Config{DataDir: data, Credentials: "unused", KopiaConfig: k.Config, Parallel: 1, Sources: []config.Source{{ID: "alpha", Plugin: wasm, SHA256: rt.Sum(b), Config: map[string]any{"message": "initial result", "files": map[string]string{"nested/file.txt": "first bytes", "removed": "old"}}}, {ID: "beta", Plugin: wasm, SHA256: rt.Sum(b), Config: map[string]any{"files": map[string]string{"secret": "beta bytes"}}}}}
	if e = cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	store, e := catalog.Open(filepath.Join(data, "catalog.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	a, e := app.New(context.Background(), cfg, store, k)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	wait := func(source string) model.Run {
		t.Helper()
		id, e := a.Trigger(source)
		if e != nil {
			t.Fatal(e)
		}
		for range 200 {
			runs, e := store.Runs(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			for _, r := range runs {
				if r.ID == id && r.Finished != nil {
					return r
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("run timeout")
		return model.Run{}
	}
	r := wait("alpha")
	if r.Status != "succeeded" || r.Message != "initial result" {
		t.Fatalf("run %+v", r)
	}
	// A committed candidate becomes current immediately, so the next check
	// does not restore the entire snapshot just to open previous artifacts.
	cached, err := os.ReadFile(filepath.Join(data, "current", "alpha", "files", "nested", "file.txt"))
	if err != nil || string(cached) != "first bytes" {
		t.Fatalf("committed current cache: %q %v", cached, err)
	}
	r = wait("beta")
	if r.Status != "succeeded" {
		t.Fatalf("run %+v", r)
	}
	first, e := store.Revision(context.Background(), "alpha", "latest")
	if e != nil {
		t.Fatal(e)
	}
	loaded := a.Sources["alpha"]
	loaded.Config.Config = map[string]any{"no_change": true, "message": "checked upstream"}
	a.Sources["alpha"] = loaded
	if r = wait("alpha"); r.Status != "no_change" || r.Message != "checked upstream" {
		t.Fatalf("nochange %+v", r)
	}
	loaded.Config.Config = map[string]any{"no_change": true, "message": strings.Repeat("x", 4097)}
	a.Sources["alpha"] = loaded
	if r = wait("alpha"); r.Status != "failed" || r.Message != "" || !strings.Contains(r.Error, "message exceeds") {
		t.Fatalf("oversized message %+v", r)
	}
	loaded.Config.Config = map[string]any{"files": map[string]string{"nested/file.txt": "failed bytes"}, "fail": true, "message": "must not appear"}
	a.Sources["alpha"] = loaded
	if r = wait("alpha"); r.Status != "failed" || r.Message != "" {
		t.Fatalf("failure %+v", r)
	}
	head, _ := store.Revision(context.Background(), "alpha", "latest")
	if head.ID != first.ID {
		t.Fatal("failure advanced state")
	}
	loaded.Config.Config = map[string]any{"files": map[string]string{"nested/file.txt": "second bytes"}, "deletes": []string{"removed"}}
	a.Sources["alpha"] = loaded
	if r = wait("alpha"); r.Status != "succeeded" {
		t.Fatalf("delta %+v", r)
	}
	latest, _ := store.Revision(context.Background(), "alpha", "latest")
	if latest.Parent != first.ID || len(latest.Files) != 1 || len(latest.Changes) != 2 {
		t.Fatalf("revision %+v", latest)
	}
	token, secret, e := store.CreateToken(context.Background(), "alpha-reader", []string{"alpha"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("test-admin"), bcrypt.MinCost)
	server := httptest.NewServer((&web.Server{App: a, Credentials: web.Credentials{Username: "admin", PasswordHash: string(hash)}}).Handler())
	defer server.Close()
	get := func(path, token string, want int, rangeHeader string) []byte {
		t.Helper()
		req, _ := http.NewRequest("GET", server.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		b, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != want {
			t.Fatalf("GET %s: %d wanted %d: %s", path, resp.StatusCode, want, b)
		}
		return b
	}
	prefix := "/api/v1/sources/alpha/revisions/"
	get("/api/v1/sources", "", 401, "")
	if b = get("/api/v1/sources", secret, 200, ""); bytes.Contains(b, []byte("beta")) {
		t.Fatal("source leaked")
	}
	get("/api/v1/sources/beta/revisions", secret, 404, "")
	if b = get(prefix+first.ID+"/files/nested/file.txt", secret, 200, ""); string(b) != "first bytes" {
		t.Fatalf("history %s", b)
	}
	if b = get(prefix+latest.ID+"/files/nested/file.txt", secret, 206, "bytes=0-5"); string(b) != "second" {
		t.Fatalf("range %s", b)
	}
	get(prefix+latest.ID+"/archive", secret, 200, "")
	get("/api/v1/runs", secret, 404, "")
	get("/ui/", secret, 401, "")
	req, _ := http.NewRequest("POST", server.URL+"/ui/tokens", strings.NewReader("name=blocked&sources=alpha"))
	req.SetBasicAuth("admin", "test-admin")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://attacker.invalid")
	resp, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("CSRF %d", resp.StatusCode)
	}
	if e = store.Revoke(context.Background(), token.ID); e != nil {
		t.Fatal(e)
	}
	get(prefix+latest.ID+"/files/nested/file.txt", secret, 401, "")
	if e = os.RemoveAll(filepath.Join(data, "current")); e != nil {
		t.Fatal(e)
	}
	current, e := a.Current(context.Background(), latest)
	if e != nil {
		t.Fatal(e)
	}
	b, e = os.ReadFile(filepath.Join(current, "files/nested/file.txt"))
	if e != nil || string(b) != "second bytes" {
		t.Fatalf("restore %s %v", b, e)
	}
	// A fresh catalog imports the same immutable snapshot chain, never token records.
	recovered, e := catalog.Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close()
	a2, e := app.New(context.Background(), cfg, recovered, k)
	if e != nil {
		t.Fatal(e)
	}
	defer a2.Close()
	rr, e := recovered.Revision(context.Background(), "alpha", "latest")
	if e != nil || rr.ID != latest.ID {
		t.Fatalf("recover %+v %v", rr, e)
	}
	tokens, e := recovered.Tokens(context.Background())
	if e != nil || len(tokens) != 0 {
		t.Fatalf("tokens restored %+v %v", tokens, e)
	}
	if e = a2.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	all, e := recovered.Revisions(context.Background(), "alpha")
	if e != nil || len(all) != 2 {
		t.Fatalf("duplicate recovery %d %v", len(all), e)
	}
	raw, _ := json.Marshal(rr)
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("token in snapshot")
	}
	fmt.Fprintln(io.Discard, raw)
}
