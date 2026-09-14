package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cca2878/crawlbox/internal/app"
	"github.com/cca2878/crawlbox/internal/catalog"
	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/kopia"
	rt "github.com/cca2878/crawlbox/internal/runtime"
	"github.com/cca2878/crawlbox/internal/testutil"
	"go.yaml.in/yaml/v3"
)

type crashAfterSnapshot struct {
	kopia.Backend
	marker string
}

func (b crashAfterSnapshot) Create(ctx context.Context, dir, source, id string) (kopia.Snapshot, error) {
	s, err := b.Backend.Create(ctx, dir, source, id)
	if err != nil {
		return s, err
	}
	if err = os.WriteFile(b.marker, []byte(id), 0600); err != nil {
		return s, err
	}
	<-ctx.Done()
	return s, ctx.Err()
}

func TestSIGKILLRecovery(t *testing.T) {
	if path := os.Getenv("CRAWLBOX_CRASH_CONFIG"); path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		store, err := catalog.Open(filepath.Join(cfg.DataDir, "catalog.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var backend kopia.Backend = &kopia.CLI{Binary: os.Getenv("KOPIA_BIN"), Config: cfg.KopiaConfig}
		if marker := os.Getenv("CRAWLBOX_CRASH_MARKER"); marker != "" {
			backend = crashAfterSnapshot{backend, marker}
		}
		a, err := app.New(context.Background(), cfg, store, backend)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if _, err = a.Trigger("crash"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		t.Fatal("parent did not kill helper")
	}
	wasm := os.Getenv("FIXTURE_WASM")
	if wasm == "" {
		t.Skip("FIXTURE_WASM required")
	}
	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"running", "snapshot"} {
		t.Run(phase, func(t *testing.T) {
			k := testutil.Kopia(t)
			data := t.TempDir()
			cfg := config.Config{DataDir: data, Credentials: "unused", KopiaConfig: k.Config, Sources: []config.Source{{ID: "crash", Plugin: wasm, SHA256: rt.Sum(b), Config: map[string]any{"wait": phase == "running", "files": map[string]string{"file": "safe bytes"}}}}}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path, marker := filepath.Join(data, "config.yaml"), filepath.Join(data, "snapshot-ready")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			log, err := os.Create(filepath.Join(data, "child.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			child := exec.Command(os.Args[0], "-test.run=^TestSIGKILLRecovery$")
			child.Stdout, child.Stderr = log, log
			child.Env = append(os.Environ(), "CRAWLBOX_CRASH_CONFIG="+path, "CRAWLBOX_CRASH_MARKER=")
			if phase == "snapshot" {
				child.Env = append(child.Env, "CRAWLBOX_CRASH_MARKER="+marker)
			}
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			store, err := catalog.Open(filepath.Join(data, "catalog.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ready := false
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
				if phase == "snapshot" {
					_, err = os.Stat(marker)
					ready = err == nil
				} else {
					runs, e := store.Runs(t.Context())
					if e != nil {
						t.Fatal(e)
					}
					for _, r := range runs {
						ready = ready || r.Progress == "fixture waiting"
					}
				}
				if ready {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				t.Fatal("crash boundary not reached")
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = child.Wait()
			if err = cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			restored, err := app.New(t.Context(), cfg, store, k)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			revisions, err := store.Revisions(t.Context(), "crash")
			if err != nil {
				t.Fatal(err)
			}
			runs, err := store.Runs(t.Context())
			if err != nil || len(runs) != 1 {
				t.Fatal("run missing", err)
			}
			if phase == "running" {
				if len(revisions) != 0 || runs[0].Status != "interrupted" {
					t.Fatal("unfinished candidate published")
				}
			} else {
				if len(revisions) != 1 || runs[0].Status != "succeeded" {
					t.Fatal("complete snapshot not reconciled")
				}
				root, err := restored.Current(t.Context(), revisions[0])
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(root, "files/file"))
				if err != nil || string(got) != "safe bytes" {
					t.Fatal("recovered bytes", err)
				}
				if err = restored.Recover(t.Context()); err != nil {
					t.Fatal(err)
				}
				revisions, err = store.Revisions(t.Context(), "crash")
				if err != nil || len(revisions) != 1 {
					t.Fatal("duplicate recovery")
				}
			}
			entries, err := os.ReadDir(filepath.Join(data, "staging"))
			if err != nil || len(entries) != 0 {
				t.Fatal("staging not recovered", err)
			}
		})
	}
}
