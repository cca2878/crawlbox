package app_test

import (
	"context"
	"errors"
	"github.com/cca2878/crawlbox/internal/app"
	"github.com/cca2878/crawlbox/internal/catalog"
	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/kopia"
	"github.com/cca2878/crawlbox/internal/model"
	rt "github.com/cca2878/crawlbox/internal/runtime"
	"github.com/cca2878/crawlbox/internal/testutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type lostAcknowledgement struct{ kopia.Backend }

func (k lostAcknowledgement) Create(ctx context.Context, dir, source, id string) (kopia.Snapshot, error) {
	s, e := k.Backend.Create(ctx, dir, source, id)
	if e != nil {
		return s, e
	}
	return s, errors.New("injected lost snapshot acknowledgement")
}
func TestRecoverSnapshotAfterLostAcknowledgement(t *testing.T) {
	wasm := os.Getenv("FIXTURE_WASM")
	if wasm == "" {
		t.Skip("FIXTURE_WASM required")
	}
	k := testutil.Kopia(t)
	b, e := os.ReadFile(wasm)
	if e != nil {
		t.Fatal(e)
	}
	c := config.Config{DataDir: t.TempDir(), Credentials: "unused", KopiaConfig: k.Config, Sources: []config.Source{{ID: "recovery", Plugin: wasm, SHA256: rt.Sum(b), Config: map[string]any{"message": "must not appear after failed snapshot", "files": map[string]string{"a": "committed only in snapshot"}}}}}
	if e = c.Validate(); e != nil {
		t.Fatal(e)
	}
	s, e := catalog.Open(filepath.Join(c.DataDir, "catalog.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	a, e := app.New(context.Background(), c, s, lostAcknowledgement{k})
	if e != nil {
		t.Fatal(e)
	}
	id, e := a.Trigger("recovery")
	if e != nil {
		t.Fatal(e)
	}
	done := false
	for range 200 {
		runs, e := s.Runs(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range runs {
			if r.ID == id && r.Finished != nil {
				if r.Status != "failed" || r.Message != "" {
					t.Fatalf("expected injected failure: %+v", r)
				}
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	a.Close()
	if !done {
		t.Fatal("timeout")
	}
	if _, e = s.Revision(context.Background(), "recovery", "latest"); e == nil {
		t.Fatal("published before acknowledgement")
	}
	next, e := app.New(context.Background(), c, s, k)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	awaitRun(t, s, id, func(r model.Run) bool { return r.Status == "succeeded" && r.Finished != nil })
	rev, e := s.Revision(context.Background(), "recovery", "latest")
	if e != nil || rev.Snapshot == "" || len(rev.Files) != 1 {
		t.Fatalf("recovered %+v %v", rev, e)
	}
	if e = next.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	revisions, e := s.Revisions(context.Background(), "recovery")
	if e != nil || len(revisions) != 1 {
		t.Fatalf("duplicate recovery %v %+v", e, revisions)
	}
}
