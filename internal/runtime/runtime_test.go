package runtime

import (
	"context"
	"example.org/crawler/manager/internal/config"
	"example.org/crawler/manager/internal/wire"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHostConfinementAndQuota(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("oversized")) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	h := NewHost(config.Source{Hosts: []string{u.Host}, StagingBytes: 4, DownloadParallel: 1}, wire.Descriptor{Hosts: []string{"127.0.0.1"}}, t.TempDir(), "", "", nil)
	defer h.Close()
	for _, p := range []string{"../x", "/x", "a/../b", "a\\b", ".", "a//b"} {
		if SafePath(p) {
			t.Errorf("unsafe %q", p)
		}
	}
	if _, e := h.Call(ctx, wire.Request{Op: "http", URL: srv.URL}); e == nil {
		t.Fatal("quota bypass")
	}
	if _, e := h.Call(ctx, wire.Request{Op: "http", URL: "http://example.com"}); e == nil {
		t.Fatal("network grant bypass")
	}
	if _, e := h.Call(ctx, wire.Request{Op: "previous_file", Path: "../catalog.sqlite"}); e == nil {
		t.Fatal("path escape")
	}
	if _, e := h.Call(ctx, wire.Request{Op: "sqlite"}); e == nil {
		t.Fatal("unexpected SQL API")
	}
}
func TestSealedObjectsAndConflicts(t *testing.T) {
	h := NewHost(config.Source{StagingBytes: 1024, DownloadParallel: 1}, wire.Descriptor{}, t.TempDir(), "", "", nil)
	defer h.Close()
	ctx := context.Background()
	r, e := h.Call(ctx, wire.Request{Op: "create"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.Call(ctx, wire.Request{Op: "write", Handle: r.Handle, Data: []byte("data")}); e != nil {
		t.Fatal(e)
	}
	if _, e = h.Call(ctx, wire.Request{Op: "publish_file", Handle: r.Handle, Path: "a"}); e == nil {
		t.Fatal("unsealed published")
	}
	if _, e = h.Call(ctx, wire.Request{Op: "close", Handle: r.Handle}); e != nil {
		t.Fatal(e)
	}
	if _, e = h.Call(ctx, wire.Request{Op: "publish_file", Handle: r.Handle, Path: "a"}); e != nil {
		t.Fatal(e)
	}
	if _, e = h.Call(ctx, wire.Request{Op: "delete_file", Path: "a"}); e == nil {
		t.Fatal("conflict accepted")
	}
	if _, e = h.Call(ctx, wire.Request{Op: "write", Handle: r.Handle, Data: []byte("mutate")}); e == nil {
		t.Fatal("sealed object changed")
	}
}
