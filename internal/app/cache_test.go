package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cca2878/crawlbox/internal/kopia"
	"os"
	"path/filepath"
	"strings"
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

// Other backend operations must not be called while preparing current.
type restoreCallback struct {
	kopia.Backend
	restore func(context.Context, string, string, string) error
}

func (b restoreCallback) Restore(ctx context.Context, id, relative, target string) error {
	return b.restore(ctx, id, relative, target)
}

func TestCurrentRestoreProgress(t *testing.T) {
	for _, outcome := range []string{"success", "corrupt", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			data := t.TempDir()
			if err := os.Mkdir(filepath.Join(data, "staging"), 0700); err != nil {
				t.Fatal(err)
			}
			r := model.Revision{ID: "rev1", Source: "alpha", Snapshot: "snapshot1", Files: []wire.Entry{{Path: "file", Size: 4, SHA256: rt.Sum([]byte("good"))}}}
			var messages []string
			report := func(message string) { messages = append(messages, message) }
			restores := 0
			backend := restoreCallback{restore: func(ctx context.Context, id, relative, target string) error {
				restores++
				// Restore may block for a long time: its stage must already be visible.
				if !strings.Contains(messages[len(messages)-1], "正在从 Kopia 恢复") {
					t.Fatal("missing restore progress before I/O")
				}
				if id != r.Snapshot || relative != "" {
					t.Fatal("wrong restore target")
				}
				if outcome == "cancelled" {
					cancel()
					return ctx.Err()
				}
				if err := os.MkdirAll(filepath.Join(target, "files"), 0700); err != nil {
					return err
				}
				content := "good"
				if outcome == "corrupt" {
					content = "oops"
				}
				if err := os.WriteFile(filepath.Join(target, "files", "file"), []byte(content), 0600); err != nil {
					return err
				}
				b, err := json.Marshal(r)
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(target, "revision.json"), b, 0600)
			}}
			a := &App{Config: config.Config{DataDir: data}, Backend: backend, cacheSlots: map[string]chan struct{}{}}
			_, err := a.current(ctx, r, report)
			joined := strings.Join(messages, "\n")
			if outcome != "success" {
				if err == nil {
					t.Fatal("failed restore accepted")
				}
				if strings.Contains(joined, "校验完成") || strings.Contains(joined, "已就绪") {
					t.Fatalf("premature success: %s", joined)
				}
				if _, err := os.Stat(filepath.Join(data, "current", "alpha")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed restore installed: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"共 1 个文件", "0/1", "1/1", "正在安装", "已就绪"} {
					if !strings.Contains(joined, want) {
						t.Fatalf("missing %s: %s", want, joined)
					}
				}
				messages = nil
				if _, err := a.current(ctx, r, report); err != nil {
					t.Fatal(err)
				}
				joined = strings.Join(messages, "\n")
				if restores != 1 || !strings.Contains(joined, "缓存可用") || strings.Contains(joined, "正在校验") {
					t.Fatalf("cache hit restored or verified: %s", joined)
				}
			}
			entries, err := os.ReadDir(filepath.Join(data, "staging"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging not cleaned: %v %v", entries, err)
			}
		})
	}
}

// sealedBackend serves one revision.json, standing in for the copy inside a
// snapshot.
type sealedBackend struct{ sealed model.Revision }

func (b sealedBackend) Create(context.Context, string, string, string) (kopia.Snapshot, error) {
	return kopia.Snapshot{}, errors.New("not used")
}
func (b sealedBackend) List(context.Context) ([]kopia.Snapshot, error) { return nil, nil }
func (b sealedBackend) Restore(_ context.Context, _, relative, target string) error {
	if relative != "revision.json" {
		return errors.New("only the revision record is read")
	}
	if e := os.MkdirAll(filepath.Dir(target), 0700); e != nil {
		return e
	}
	raw, e := json.Marshal(b.sealed)
	if e != nil {
		return e
	}
	return os.WriteFile(target, raw, 0600)
}

// TestConfirmRevisionDetectsDrift covers the check that runs before a catalog
// record is used. Nothing revisits a record once written, so a drifted digest
// would be inherited into the next revision and sealed into history, where it
// can no longer be corrected.
func TestConfirmRevisionDetectsDrift(t *testing.T) {
	sealed := model.Revision{
		ID: "rev1", Source: "alpha", Parent: "rev0", State: json.RawMessage(`{"cursor":1}`),
		Files:     []wire.Entry{{Path: "a", Size: 3, SHA256: strings.Repeat("a", 64)}},
		Artifacts: []wire.Entry{{Path: "index.json", Size: 2, SHA256: strings.Repeat("b", 64)}},
	}
	newApp := func() *App {
		data := t.TempDir()
		if e := os.MkdirAll(filepath.Join(data, "staging"), 0700); e != nil {
			t.Fatal(e)
		}
		return &App{Config: config.Config{DataDir: data}, Backend: sealedBackend{sealed: sealed},
			cacheSlots: map[string]chan struct{}{}}
	}
	// The catalog copy carries the snapshot identifier, which the sealed copy
	// cannot: it is assigned after the record is written into the tree.
	faithful := sealed
	faithful.Snapshot = "snap1"
	if e := newApp().confirmRevision(t.Context(), faithful); e != nil {
		t.Fatalf("a faithful record must pass: %v", e)
	}

	for name, mutate := range map[string]func(*model.Revision){
		"file digest": func(r *model.Revision) {
			r.Files = []wire.Entry{{Path: "a", Size: 3, SHA256: strings.Repeat("c", 64)}}
		},
		"artifact digest": func(r *model.Revision) {
			r.Artifacts = []wire.Entry{{Path: "index.json", Size: 2, SHA256: strings.Repeat("c", 64)}}
		},
		"file added": func(r *model.Revision) {
			r.Files = append(append([]wire.Entry(nil), r.Files...), wire.Entry{Path: "b", Size: 1})
		},
		"plugin state": func(r *model.Revision) { r.State = json.RawMessage(`{"cursor":2}`) },
		"parent":       func(r *model.Revision) { r.Parent = "rev-other" },
	} {
		t.Run(name, func(t *testing.T) {
			drifted := faithful
			mutate(&drifted)
			if e := newApp().confirmRevision(t.Context(), drifted); e == nil {
				t.Fatalf("drift in %s was not detected", name)
			}
		})
	}

	t.Run("no snapshot", func(t *testing.T) {
		orphan := sealed
		if e := newApp().confirmRevision(t.Context(), orphan); e == nil {
			t.Fatal("a record with no snapshot cannot be confirmed")
		}
	})
}
