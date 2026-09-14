package app_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cca2878/crawlbox/internal/app"
	"github.com/cca2878/crawlbox/internal/catalog"
	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/kopia"
	"github.com/cca2878/crawlbox/internal/model"
	rt "github.com/cca2878/crawlbox/internal/runtime"
	"github.com/cca2878/crawlbox/internal/testutil"
	"github.com/cca2878/crawlbox/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func awaitRun(t *testing.T, store *catalog.Store, id string, match func(model.Run) bool) model.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := store.Runs(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.ID == id && match(r) {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run wait timed out", id)
	return model.Run{}
}

func cancelFixture(t *testing.T, backend kopia.Backend) (*app.App, *catalog.Store, config.Config) {
	t.Helper()
	wasm := os.Getenv("FIXTURE_WASM")
	if wasm == "" {
		t.Skip("FIXTURE_WASM required")
	}
	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: t.TempDir(), Credentials: "unused", KopiaConfig: "unused", Parallel: 1, Sources: []config.Source{
		{ID: "waiting", Plugin: wasm, SHA256: rt.Sum(b), Timeout: time.Minute, Config: map[string]any{"wait": true, "files": map[string]string{"uncommitted": "bytes"}}},
		{ID: "normal", Plugin: wasm, SHA256: rt.Sum(b), Timeout: time.Minute, Config: map[string]any{"message": "publication complete", "files": map[string]string{"published": "bytes"}}},
	}}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := catalog.Open(filepath.Join(cfg.DataDir, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a, err := app.New(t.Context(), cfg, store, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, store, cfg
}

func TestCancelQueuedAndRunning(t *testing.T) {
	if os.Getenv("FIXTURE_WASM") == "" {
		t.Skip("FIXTURE_WASM required")
	}
	k := testutil.Kopia(t)
	a, store, cfg := cancelFixture(t, k)
	first, err := a.Trigger("waiting")
	if err != nil {
		t.Fatal(err)
	}
	awaitRun(t, store, first, func(r model.Run) bool { return r.Progress == "fixture waiting" })
	queued, err := a.Trigger("normal")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	handler := (&web.Server{App: a, Credentials: web.Credentials{Username: "admin", PasswordHash: string(hash)}}).Handler()
	request := func(method, path, origin string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if auth {
			r.SetBasicAuth("admin", "password")
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := "/ui/runs/" + queued + "/cancel"
	if w := request("POST", path, "", false); w.Code != 401 {
		t.Fatal("cancel bypassed auth", w.Code)
	}
	if w := request("POST", path, "https://attacker.invalid", true); w.Code != 403 {
		t.Fatal("cancel bypassed CSRF", w.Code)
	}
	if w := request("GET", path, "", true); w.Code != 405 {
		t.Fatal("cancel accepted GET", w.Code)
	}
	if w := request("POST", path, "", true); w.Code != 303 || w.Header().Get("Location") != "/ui/?page=runs" {
		t.Fatal("UI cancel failed", w.Code)
	}
	if r := awaitRun(t, store, queued, func(r model.Run) bool { return r.Finished != nil }); r.Status != "interrupted" {
		t.Fatal(r)
	}
	if !a.CanCancel(first) {
		t.Fatal("cancel affected another source")
	}
	if err = a.Cancel(first); err != nil {
		t.Fatal(err)
	}
	if r := awaitRun(t, store, first, func(r model.Run) bool { return r.Finished != nil }); r.Status != "interrupted" {
		t.Fatal(r)
	}
	if _, err = store.Revision(t.Context(), "waiting", "latest"); err == nil {
		t.Fatal("cancelled candidate committed")
	}
	// Wait for the completed goroutine's source release, then start a new run.
	var next string
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		next, err = a.Trigger("normal")
		if err != nil {
			t.Fatal(err)
		}
		if next != queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if next == queued {
		t.Fatal("source was not released")
	}
	if err = a.Cancel(queued); !errors.Is(err, app.ErrRunNotActive) {
		t.Fatal("stale ID can cancel newer run", err)
	}
	if r := awaitRun(t, store, next, func(r model.Run) bool { return r.Finished != nil }); r.Status != "succeeded" {
		t.Fatal(r)
	}
	a.Close()
	entries, err := os.ReadDir(filepath.Join(cfg.DataDir, "staging"))
	if err != nil || len(entries) != 0 {
		t.Fatal("staging not cleaned", err, len(entries))
	}
}

type pausedSnapshot struct {
	kopia.Backend
	entered chan struct{}
	release chan struct{}
}

func (p *pausedSnapshot) Create(ctx context.Context, root, source, id string) (kopia.Snapshot, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return kopia.Snapshot{}, ctx.Err()
	}
	return p.Backend.Create(ctx, root, source, id)
}
func TestCancelCannotCrossPublication(t *testing.T) {
	if os.Getenv("FIXTURE_WASM") == "" {
		t.Skip("FIXTURE_WASM required")
	}
	k := testutil.Kopia(t)
	backend := &pausedSnapshot{Backend: k, entered: make(chan struct{}), release: make(chan struct{})}
	a, store, _ := cancelFixture(t, backend)
	id, err := a.Trigger("normal")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-backend.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("snapshot not reached")
	}
	if r := awaitRun(t, store, id, func(r model.Run) bool { return r.Status == "snapshotting" }); r.Message != "" {
		t.Fatal("message published before commit", r)
	}
	if a.CanCancel(id) {
		t.Fatal("snapshot still cancellable")
	}
	if err = a.Cancel(id); !errors.Is(err, app.ErrRunSealed) {
		t.Fatal("snapshot cancel accepted", err)
	}
	close(backend.release)
	if r := awaitRun(t, store, id, func(r model.Run) bool { return r.Finished != nil }); r.Status != "succeeded" || r.Message != "publication complete" {
		t.Fatal(r)
	}
}

func TestShutdownInterruptsRun(t *testing.T) {
	if os.Getenv("FIXTURE_WASM") == "" {
		t.Skip("FIXTURE_WASM required")
	}
	k := testutil.Kopia(t)
	a, store, _ := cancelFixture(t, k)
	id, err := a.Trigger("waiting")
	if err != nil {
		t.Fatal(err)
	}
	awaitRun(t, store, id, func(r model.Run) bool { return r.Progress == "fixture waiting" })
	a.Close()
	if r := awaitRun(t, store, id, func(r model.Run) bool { return r.Finished != nil }); r.Status != "interrupted" {
		t.Fatal(r)
	}
	if _, err = store.Revision(t.Context(), "waiting", "latest"); err == nil {
		t.Fatal("shutdown committed candidate")
	}
	if _, err = a.Trigger("normal"); err == nil {
		t.Fatal("closed app accepted a run")
	}
}
