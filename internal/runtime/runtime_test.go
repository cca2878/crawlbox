package runtime

import (
	"bytes"
	"context"
	"fmt"
	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/wire"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRangedResponseHeaders covers the response side of ranged requests. A
// plugin can already send Range; without Content-Range coming back it cannot
// tell where the returned bytes sit in the object, because a partial response
// reports the full size nowhere else.
func TestRangedResponseHeaders(t *testing.T) {
	body := []byte("0123456789abcdef")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "object.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	h := NewHost(config.Source{Hosts: []string{u.Host}, AllowPrivateTargets: true, StagingBytes: 1024, DownloadParallel: 1},
		wire.Descriptor{Hosts: []string{"127.0.0.1"}}, t.TempDir(), Previous{}, nil)
	defer h.Close()
	r, e := h.Call(context.Background(), wire.Request{Op: "http", URL: srv.URL,
		Headers: map[string]string{"Range": "bytes=-4"}})
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != http.StatusPartialContent {
		t.Fatalf("status %d", r.Status)
	}
	if got := r.Headers["Content-Range"]; got != "bytes 12-15/16" {
		t.Fatalf("Content-Range %q", got)
	}
	if r.Size != 4 {
		t.Fatalf("staged %d bytes", r.Size)
	}
	// Headers are forwarded in full rather than by an allowlist, so a plugin
	// needing a new one does not need a Manager change.
	if got := r.Headers["Content-Type"]; got == "" {
		t.Fatal("ordinary headers must be forwarded too")
	}
}

// TestResponseHeaderCaps covers the bound on what a hostile upstream can make
// the host build. The caps are resource protection, not confidentiality.
func TestResponseHeaderCaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < maxResponseHeaders*3; i++ {
			w.Header().Set(fmt.Sprintf("X-Filler-%04d", i), "v")
		}
		w.Header().Set("X-Huge", strings.Repeat("a", maxResponseHeaderBytes*2))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	h := NewHost(config.Source{Hosts: []string{u.Host}, AllowPrivateTargets: true, StagingBytes: 1024, DownloadParallel: 1},
		wire.Descriptor{Hosts: []string{"127.0.0.1"}}, t.TempDir(), Previous{}, nil)
	defer h.Close()
	r, e := h.Call(context.Background(), wire.Request{Op: "http", URL: srv.URL})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Headers) > maxResponseHeaders {
		t.Fatalf("forwarded %d headers", len(r.Headers))
	}
	for k, v := range r.Headers {
		if len(v) > maxResponseHeaderBytes {
			t.Fatalf("header %s is %d bytes", k, len(v))
		}
	}
}

func TestHostConfinementAndQuota(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("oversized")) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	h := NewHost(config.Source{Hosts: []string{u.Host}, AllowPrivateTargets: true, StagingBytes: 4, DownloadParallel: 1}, wire.Descriptor{Hosts: []string{"127.0.0.1"}}, t.TempDir(), Previous{}, nil)
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
	h := NewHost(config.Source{AllowPrivateTargets: true, StagingBytes: 1024, DownloadParallel: 1}, wire.Descriptor{}, t.TempDir(), Previous{}, nil)
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

// TestInheritedMetadataComesFromCatalog pins that the previous view is
// answered from the committed entries rather than by hashing the tree again.
// The files on disk carry content that does not match their committed digest:
// anything that re-reads bytes would report the recomputed value instead.
func TestInheritedMetadataComesFromCatalog(t *testing.T) {
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	if e := os.MkdirAll(files, 0700); e != nil {
		t.Fatal(e)
	}
	body := []byte("bytes on disk")
	if e := os.WriteFile(filepath.Join(files, "a.txt"), body, 0600); e != nil {
		t.Fatal(e)
	}
	committed := wire.Entry{Path: "a.txt", Size: int64(len(body)), SHA256: strings.Repeat("c", 64)}
	h := NewHost(config.Source{AllowPrivateTargets: true, StagingBytes: 1024, DownloadParallel: 1}, wire.Descriptor{}, dir,
		Previous{Files: files, Entries: []wire.Entry{committed}}, nil)
	defer h.Close()
	ctx := context.Background()

	r, e := h.Call(ctx, wire.Request{Op: "list_files"})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Entries) != 1 || r.Entries[0] != committed {
		t.Fatalf("listing rebuilt metadata instead of using the catalog: %+v", r.Entries)
	}

	// stat on an inherited object must answer from the same record.
	r, e = h.Call(ctx, wire.Request{Op: "previous_file", Path: "a.txt"})
	if e != nil {
		t.Fatal(e)
	}
	st, e := h.Call(ctx, wire.Request{Op: "stat", Handle: r.Handle})
	if e != nil {
		t.Fatal(e)
	}
	if st.SHA256 != committed.SHA256 || st.Size != committed.Size {
		t.Fatalf("stat rehashed an inherited object: %+v", st)
	}

	// A staged object has no committed record, so it is still hashed.
	c, e := h.Call(ctx, wire.Request{Op: "create"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.Call(ctx, wire.Request{Op: "write", Handle: c.Handle, Data: body}); e != nil {
		t.Fatal(e)
	}
	st, e = h.Call(ctx, wire.Request{Op: "stat", Handle: c.Handle})
	if e != nil {
		t.Fatal(e)
	}
	if st.SHA256 != Sum(body) {
		t.Fatalf("staged object must be hashed: %+v", st)
	}
}

// TestInheritedPagination covers offset and limit over the committed entries.
func TestInheritedPagination(t *testing.T) {
	var entries []wire.Entry
	for _, name := range []string{"c", "a", "b"} {
		entries = append(entries, wire.Entry{Path: name, Size: 1, SHA256: strings.Repeat(name, 64)})
	}
	h := NewHost(config.Source{AllowPrivateTargets: true, StagingBytes: 1, DownloadParallel: 1}, wire.Descriptor{}, t.TempDir(),
		Previous{Files: t.TempDir(), Entries: entries}, nil)
	defer h.Close()
	r, e := h.Call(context.Background(), wire.Request{Op: "list_files", Offset: 1, Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	// Entries are served in path order regardless of how they arrived.
	if len(r.Entries) != 1 || r.Entries[0].Path != "b" {
		t.Fatalf("pagination over sorted entries: %+v", r.Entries)
	}
}

// TestPrivateTargetsNeedAnExplicitGrant pins the boundary as a boundary. A
// literal address is not an exemption: writing 127.0.0.1 instead of a name that
// resolves to it is the same request, so a check that trusts the literal form
// can be stepped around by anyone who can choose the URL.
func TestPrivateTargetsNeedAnExplicitGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("reached"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	grant := config.Source{Hosts: []string{u.Host}, StagingBytes: 1024, DownloadParallel: 1}
	desc := wire.Descriptor{Hosts: []string{"127.0.0.1"}}

	// Granting the host authorises which upstream may be reached; it does not
	// authorise reaching inward.
	denied := NewHost(grant, desc, t.TempDir(), Previous{}, nil)
	defer denied.Close()
	if _, e := denied.Call(context.Background(), wire.Request{Op: "http", URL: srv.URL}); e == nil {
		t.Fatal("a literal private address was reached without an explicit grant")
	}

	allowed := grant
	allowed.AllowPrivateTargets = true
	h := NewHost(allowed, desc, t.TempDir(), Previous{}, nil)
	defer h.Close()
	r, e := h.Call(context.Background(), wire.Request{Op: "http", URL: srv.URL})
	if e != nil || r.Status != http.StatusOK {
		t.Fatalf("an explicit grant must permit the target: %v %+v", e, r)
	}
}
