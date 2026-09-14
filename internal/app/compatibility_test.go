package app_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cca2878/crawlbox/internal/app"
	"github.com/cca2878/crawlbox/internal/catalog"
	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/model"
	rt "github.com/cca2878/crawlbox/internal/runtime"
	"github.com/cca2878/crawlbox/internal/testutil"
	"github.com/cca2878/crawlbox/internal/wire"
)

func TestRecoverLegacyAndIncrementalSnapshotHistory(t *testing.T) {
	k := testutil.Kopia(t)
	for _, id := range []string{"legacy", "incremental"} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "files"), 0700); err != nil {
			t.Fatal(err)
		}
		content := []byte(id + " bytes")
		if err := os.WriteFile(filepath.Join(dir, "files", "data"), content, 0600); err != nil {
			t.Fatal(err)
		}
		r := model.Revision{ID: id, Source: "alpha", State: json.RawMessage(`{"cursor":1}`), Files: []wire.Entry{{Path: "data", Size: int64(len(content)), SHA256: rt.Sum(content)}}}
		if id == "incremental" {
			r.Parent = "legacy"
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "revision.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		if id == "legacy" {
			// Exact pre-upgrade creation flags: transient physical source path and
			// forced hashing, with the existing business tags and pin.
			cmd := exec.CommandContext(t.Context(), k.Binary, "--config-file", k.Config, "--no-progress", "--disable-file-logging", "snapshot", "create", dir, "--json", "--fail-fast", "--force-hash=100", "--force-disable-actions", "--pin=business", "--tags=app:manager-v1", "--tags=source:alpha", "--tags=revision:legacy")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("legacy snapshot: %v\n%s", err, out)
			}
		} else if _, err := k.Create(t.Context(), dir, r.Source, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	data := t.TempDir()
	store, err := catalog.Open(filepath.Join(data, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, err := app.New(t.Context(), config.Config{DataDir: data, Parallel: 1}, store, k)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, id := range []string{"legacy", "incremental"} {
		r, err := store.Revision(t.Context(), "alpha", id)
		if err != nil {
			t.Fatal(err)
		}
		if string(r.State) != `{"cursor":1}` {
			t.Fatalf("state lost: %s", r.State)
		}
		root, err := a.Current(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(root, "files", "data"))
		if err != nil || string(b) != id+" bytes" {
			t.Fatalf("%s: %q %v", id, b, err)
		}
	}
	head, err := store.Revision(t.Context(), "alpha", "latest")
	if err != nil || head.ID != "incremental" || head.Parent != "legacy" {
		t.Fatalf("history: %+v %v", head, err)
	}
	if err := a.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
}
