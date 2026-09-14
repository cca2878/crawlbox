package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/model"
	rt "github.com/cca2878/crawlbox/internal/runtime"
	"github.com/cca2878/crawlbox/internal/wire"
)

func TestCurrentCacheHitDoesNotReadResourceTree(t *testing.T) {
	a := &App{Config: config.Config{DataDir: t.TempDir()}, cacheSlots: map[string]chan struct{}{}}
	r := model.Revision{ID: "rev1", Source: "alpha", Files: []wire.Entry{{Path: "not-read", Size: 12, SHA256: "committed-digest"}}}
	root := filepath.Join(a.Config.DataDir, "current", r.Source)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(root, "revision.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit the resource: a cache hit trusts installed immutable
	// contents rather than walking or hashing them. External edits are unsupported.
	got, err := a.Current(t.Context(), r)
	if err != nil || got != root {
		t.Fatalf("current %q: %v", got, err)
	}
}

func TestCurrentLocksAreSourceScopedAndCancellable(t *testing.T) {
	a := &App{cacheSlots: map[string]chan struct{}{}}
	release, err := a.lockCurrent(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	releaseBeta, err := a.lockCurrent(ctx, "beta")
	if err != nil {
		t.Fatalf("other source blocked: %v", err)
	}
	releaseBeta()
	cancel()
	if _, err := a.lockCurrent(ctx, "alpha"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}

func TestBuildReusesInheritedDigestAndHashesReplacement(t *testing.T) {
	previous := t.TempDir()
	if err := os.MkdirAll(filepath.Join(previous, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"inherited", "changed"} {
		if err := os.WriteFile(filepath.Join(previous, "files", p), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Sentinel digest distinguishes catalog reuse from a fresh read/hash.
	prev := model.Revision{Files: []wire.Entry{{Path: "inherited", Size: 3, SHA256: "committed-digest"}, {Path: "changed", Size: 3, SHA256: rt.Sum([]byte("old"))}}}
	object := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(object, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	c := rt.Candidate{Files: map[string]string{"changed": object}}
	r, err := Build(t.TempDir(), previous, prev, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || r.Changes[0].Path != "changed" {
		t.Fatalf("changes %+v", r.Changes)
	}
	for _, f := range r.Files {
		want := "committed-digest"
		if f.Path == "changed" {
			want = rt.Sum([]byte("new"))
		}
		if f.SHA256 != want {
			t.Fatalf("entry %+v", f)
		}
	}
	b, err := os.ReadFile(filepath.Join(previous, "files", "changed"))
	if err != nil || string(b) != "old" {
		t.Fatalf("baseline changed: %s %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(previous, "files", "inherited"), []byte("bad size"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(t.TempDir(), previous, prev, c); err == nil {
		t.Fatal("inherited size mismatch accepted")
	}
}
