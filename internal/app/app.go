package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"example.org/crawler/manager/internal/catalog"
	"example.org/crawler/manager/internal/config"
	"example.org/crawler/manager/internal/kopia"
	"example.org/crawler/manager/internal/model"
	rt "example.org/crawler/manager/internal/runtime"
	"example.org/crawler/manager/internal/wire"
	"fmt"
	"github.com/robfig/cron/v3"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Loaded struct {
	Config     config.Source
	Descriptor wire.Descriptor
	Wasm       []byte
}
type App struct {
	Config  config.Config
	Store   *catalog.Store
	Backend kopia.Backend
	Sources map[string]Loaded
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	active  map[string]string
	blocked map[string]bool
	slots   chan struct{}
	wg      sync.WaitGroup
	cron    *cron.Cron
	entries map[string]cron.EntryID
	cacheMu sync.Mutex
}

func New(ctx context.Context, c config.Config, store *catalog.Store, backend kopia.Backend) (*App, error) {
	ctx, cancel := context.WithCancel(ctx)
	a := &App{Config: c, Store: store, Backend: backend, Sources: map[string]Loaded{}, ctx: ctx, cancel: cancel, active: map[string]string{}, blocked: map[string]bool{}, slots: make(chan struct{}, c.Parallel), cron: cron.New(), entries: map[string]cron.EntryID{}}
	for _, p := range []string{"staging", "current", "cache"} {
		if e := os.MkdirAll(filepath.Join(c.DataDir, p), 0700); e != nil {
			cancel()
			return nil, e
		}
	}
	identities := map[string]string{}
	for _, s := range c.Sources {
		d, b, e := rt.Load(ctx, s)
		if e != nil {
			cancel()
			return nil, fmt.Errorf("load %s: %w", s.ID, e)
		}
		a.Sources[s.ID] = Loaded{s, d, b}
		slog.Info("source loaded", "source", s.ID, "plugin", d.ID)
		identities[s.ID] = d.ID
	}
	if e := a.Recover(ctx); e != nil {
		cancel()
		return nil, e
	}
	if e := store.Register(ctx, identities); e != nil {
		cancel()
		return nil, e
	}
	if e := store.InterruptRuns(ctx); e != nil {
		cancel()
		return nil, e
	}
	// No candidate or restore directory is needed after recovery finishes.
	if entries, e := os.ReadDir(filepath.Join(c.DataDir, "staging")); e == nil {
		for _, entry := range entries {
			if e = os.RemoveAll(filepath.Join(c.DataDir, "staging", entry.Name())); e != nil {
				cancel()
				return nil, e
			}
		}
	}
	return a, nil
}
func (a *App) Start() error {
	for id, s := range a.Sources {
		if s.Config.Schedule == "" {
			continue
		}
		id := id
		entry, e := a.cron.AddFunc("CRON_TZ="+s.Config.Timezone+" "+s.Config.Schedule, func() {
			if _, err := a.Trigger(id); err != nil {
				slog.Warn("scheduled collection could not be queued", "source", id)
			}
		})
		if e != nil {
			return e
		}
		a.entries[id] = entry
	}
	a.cron.Start()
	for id := range a.entries {
		slog.Info("source scheduled", "source", id, "next_run", a.Next(id))
	}
	return nil
}
func (a *App) Close()                   { a.cancel(); <-a.cron.Stop().Done(); a.wg.Wait() }
func (a *App) Next(id string) time.Time { return a.cron.Entry(a.entries[id]).Next }
func (a *App) Trigger(source string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.Sources[source]; !ok {
		return "", os.ErrNotExist
	}
	if a.ctx.Err() != nil {
		return "", a.ctx.Err()
	}
	if a.blocked[source] {
		return "", errors.New("source requires restart recovery")
	}
	if id := a.active[source]; id != "" {
		return id, nil
	}
	r := model.Run{ID: catalog.ID(), Source: source, Status: "queued", Started: time.Now().UTC()}
	if e := a.Store.SaveRun(a.ctx, r); e != nil {
		return "", e
	}
	a.active[source] = r.ID
	slog.Info("collection queued", "source", source, "run", r.ID)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() { a.mu.Lock(); delete(a.active, source); a.mu.Unlock() }()
		a.perform(r)
	}()
	return r.ID, nil
}
func (a *App) perform(r model.Run) {
	ctx, cancel := context.WithTimeout(a.ctx, a.Sources[r.Source].Config.Timeout)
	defer cancel()
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func(source, run string, started time.Time) {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ticker.C:
				slog.Info("collection still active", "source", source, "run", run, "elapsed", time.Since(started))
			}
		}
	}(r.Source, r.ID, r.Started)
	var e error
	defer func() {
		if p := recover(); p != nil {
			e = fmt.Errorf("run panic: %v", p)
		}
		if e != nil {
			r.Status = "failed"
			r.Error = e.Error()
		}
		now := time.Now().UTC()
		r.Finished = &now
		if err := a.Store.SaveRun(context.Background(), r); err != nil {
			slog.Error("could not persist final run status", "source", r.Source, "run", r.ID)
		}
		level := slog.LevelInfo
		if e != nil {
			level = slog.LevelError
		}
		slog.Log(context.Background(), level, "collection finished", "source", r.Source, "run", r.ID, "status", r.Status, "duration", now.Sub(r.Started))
		if e != nil {
			slog.Error("collection failed; details available in authenticated UI", "source", r.Source, "run", r.ID)
		}
	}()
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	case <-ctx.Done():
		e = ctx.Err()
		return
	}
	stage := func(status string) error {
		r.Status = status
		slog.Info("collection stage", "source", r.Source, "run", r.ID, "stage", status)
		return a.Store.SaveRun(ctx, r)
	}
	if e = stage("running"); e != nil {
		return
	}
	s := a.Sources[r.Source]
	var prev model.Revision
	prev, e = a.Store.Revision(ctx, r.Source, "latest")
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return
	}
	e = nil
	var previous string
	if prev.ID != "" {
		previous, e = a.Current(ctx, prev)
		if e != nil {
			return
		}
	}
	dir, err := os.MkdirTemp(filepath.Join(a.Config.DataDir, "staging"), "run-")
	if err != nil {
		e = err
		return
	}
	defer os.RemoveAll(dir)
	objects := filepath.Join(dir, "objects")
	if e = os.MkdirAll(objects, 0700); e != nil {
		return
	}
	art := ""
	files := ""
	if previous != "" {
		art = filepath.Join(previous, "artifacts")
		files = filepath.Join(previous, "files")
	}
	h := rt.NewHost(s.Config, s.Descriptor, objects, files, art, func(p string) {
		r.Progress = p
		_ = a.Store.SaveRun(ctx, r)
		slog.Debug("plugin progress updated", "source", r.Source, "run", r.ID)
	})
	defer h.Close()
	conf, _ := json.Marshal(s.Config.Config)
	in := wire.Input{ABI: 1, Source: r.Source, Run: r.ID, Config: conf, State: prev.State, PreviousRevision: prev.ID}
	candidate, err := rt.Execute(ctx, s.Config, s.Wasm, in, h)
	if err != nil {
		e = err
		return
	}
	if candidate.Result.Status == "no_change" {
		r.Status = "no_change"
		return
	}
	if e = stage("validating"); e != nil {
		return
	}
	root := filepath.Join(dir, "tree")
	rev, err := Build(root, previous, prev, candidate)
	if err != nil {
		e = err
		return
	}
	rev.ID = catalog.ID()
	rev.Source = r.Source
	rev.Run = r.ID
	rev.Parent = prev.ID
	rev.CreatedAt = time.Now().UTC()
	rev.Plugin = s.Descriptor
	rev.State = candidate.Result.State
	rev.Metadata = candidate.Result.Metadata
	rev.Tags = candidate.Result.Tags
	b, err := json.Marshal(rev)
	if err != nil {
		e = err
		return
	}
	if e = writeFile(filepath.Join(root, "revision.json"), b); e != nil {
		return
	}
	if e = stage("snapshotting"); e != nil {
		return
	}
	snap, err := a.Backend.Create(ctx, root, r.Source, rev.ID)
	if err != nil {
		a.mu.Lock()
		a.blocked[r.Source] = true
		a.mu.Unlock()
		e = err
		return
	}
	rev.Snapshot = snap.ID
	if e = stage("committing"); e != nil {
		a.block(r.Source)
		return
	}
	if e = a.Store.Commit(ctx, rev); e != nil {
		a.block(r.Source)
		return
	}
	r.Status = "succeeded"
	slog.Info("revision committed", "source", r.Source, "run", r.ID, "revision", rev.ID, "files", len(rev.Files), "artifacts", len(rev.Artifacts))
}
func (a *App) block(id string) { a.mu.Lock(); a.blocked[id] = true; a.mu.Unlock() }
func writeFile(p string, b []byte) error {
	f, e := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func copyFile(src, dst string) error {
	if e := os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return e
	}
	if e := os.Link(src, dst); e == nil {
		return nil
	}
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	ce := out.Close()
	if e != nil {
		return e
	}
	return ce
}
func Build(root, previous string, prev model.Revision, c rt.Candidate) (model.Revision, error) {
	var r model.Revision
	r.Files = []wire.Entry{}
	r.Artifacts = []wire.Entry{}
	r.Changes = []model.Change{}
	for _, dir := range []string{"files", "artifacts"} {
		if e := os.MkdirAll(filepath.Join(root, dir), 0700); e != nil {
			return r, e
		}
	}
	old := map[string]wire.Entry{}
	for _, f := range prev.Files {
		old[f.Path] = f
	}
	for p := range c.Deletes {
		if _, ok := old[p]; !ok {
			return r, fmt.Errorf("delete absent path %s", p)
		}
	}
	paths := map[string]string{}
	for p := range old {
		if !c.Deletes[p] {
			paths[p] = filepath.Join(previous, "files", filepath.FromSlash(p))
		}
	}
	for p, v := range c.Files {
		paths[p] = v
	}
	keys := make([]string, 0, len(paths))
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		if !rt.SafePath(p) {
			return r, errors.New("invalid candidate path")
		}
		dst := filepath.Join(root, "files", filepath.FromSlash(p))
		if e := copyFile(paths[p], dst); e != nil {
			return r, e
		}
		entry, e := rt.FileEntry(dst, p)
		if e != nil {
			return r, e
		}
		r.Files = append(r.Files, entry)
		o, ok := old[p]
		if !ok {
			r.Changes = append(r.Changes, model.Change{Path: p, Kind: "added", After: entry.SHA256})
		} else if o.SHA256 != entry.SHA256 {
			r.Changes = append(r.Changes, model.Change{Path: p, Kind: "modified", Before: o.SHA256, After: entry.SHA256})
		}
	}
	for p := range c.Deletes {
		r.Changes = append(r.Changes, model.Change{Path: p, Kind: "deleted", Before: old[p].SHA256})
	}
	sort.Slice(r.Changes, func(i, j int) bool { return r.Changes[i].Path < r.Changes[j].Path })
	keys = nil
	for p := range c.Artifacts {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		if !rt.SafePath(p) {
			return r, errors.New("invalid artifact path")
		}
		dst := filepath.Join(root, "artifacts", filepath.FromSlash(p))
		if e := copyFile(c.Artifacts[p], dst); e != nil {
			return r, e
		}
		entry, e := rt.FileEntry(dst, p)
		if e != nil {
			return r, e
		}
		r.Artifacts = append(r.Artifacts, entry)
	}
	r.ArtifactChanges = diff(prev.Artifacts, r.Artifacts)
	return r, nil
}
func diff(old, new []wire.Entry) []model.Change {
	before := map[string]wire.Entry{}
	for _, f := range old {
		before[f.Path] = f
	}
	out := []model.Change{}
	for _, f := range new {
		b, ok := before[f.Path]
		if !ok {
			out = append(out, model.Change{Path: f.Path, Kind: "added", After: f.SHA256})
		} else if b.SHA256 != f.SHA256 {
			out = append(out, model.Change{Path: f.Path, Kind: "modified", Before: b.SHA256, After: f.SHA256})
		}
		delete(before, f.Path)
	}
	for _, f := range before {
		out = append(out, model.Change{Path: f.Path, Kind: "deleted", Before: f.SHA256})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
func verify(root string, r model.Revision) error {
	for dir, entries := range map[string][]wire.Entry{"files": r.Files, "artifacts": r.Artifacts} {
		seen := map[string]bool{}
		for _, f := range entries {
			if !rt.SafePath(f.Path) || seen[f.Path] {
				return errors.New("invalid snapshot index")
			}
			seen[f.Path] = true
			p := filepath.Join(root, dir, filepath.FromSlash(f.Path))
			st, e := os.Lstat(p)
			if e != nil {
				return e
			}
			if !st.Mode().IsRegular() {
				return errors.New("nonregular snapshot entry")
			}
			got, e := rt.FileEntry(p, f.Path)
			if e != nil {
				return e
			}
			if got != f {
				return errors.New("snapshot content mismatch")
			}
		}
	}
	return nil
}
func (a *App) Current(ctx context.Context, r model.Revision) (string, error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	dst := filepath.Join(a.Config.DataDir, "current", r.Source)
	if b, e := os.ReadFile(filepath.Join(dst, "revision.json")); e == nil {
		var cached model.Revision
		if json.Unmarshal(b, &cached) == nil && cached.ID == r.ID {
			if e = verify(dst, r); e == nil {
				return dst, nil
			}
		}
	}
	tmp, e := os.MkdirTemp(filepath.Join(a.Config.DataDir, "staging"), "restore-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(tmp)
	tree := filepath.Join(tmp, "tree")
	if e = a.Backend.Restore(ctx, r.Snapshot, "", tree); e != nil {
		return "", e
	}
	if e = verify(tree, r); e != nil {
		return "", e
	}
	if e = os.RemoveAll(dst); e != nil {
		return "", e
	}
	if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return "", e
	}
	if e = os.Rename(tree, dst); e != nil {
		return "", e
	}
	return dst, nil
}
func (a *App) Recover(ctx context.Context) error {
	slog.Info("history recovery started")
	snaps, e := a.Backend.List(ctx)
	if e != nil {
		return e
	}
	pending := map[string]model.Revision{}
	for _, s := range snaps {
		if !s.Complete() || s.Tags["app"] != "manager-v1" {
			continue
		}
		src, id := s.Tags["source"], s.Tags["revision"]
		if !config.ValidID(src) || !config.ValidID(id) {
			return errors.New("invalid snapshot identity")
		}
		if _, e = a.Store.Revision(ctx, src, id); e == nil {
			continue
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		dir, e := os.MkdirTemp(filepath.Join(a.Config.DataDir, "staging"), "recover-")
		if e != nil {
			return e
		}
		p := filepath.Join(dir, "revision.json")
		e = a.Backend.Restore(ctx, s.ID, "revision.json", p)
		if e != nil {
			os.RemoveAll(dir)
			return e
		}
		b, e := os.ReadFile(p)
		os.RemoveAll(dir)
		if e != nil {
			return e
		}
		var r model.Revision
		if e = json.Unmarshal(b, &r); e != nil {
			return e
		}
		if r.ID != id || r.Source != src {
			return errors.New("snapshot identity mismatch")
		}
		r.Snapshot = s.ID
		if existing, ok := pending[id]; ok && existing.Snapshot != s.ID {
			return errors.New("duplicate snapshot revision identity")
		}
		pending[id] = r
	}
	recovered := len(pending)
	for len(pending) > 0 {
		advanced := false
		for id, r := range pending {
			head, e := a.Store.Revision(ctx, r.Source, "latest")
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if head.ID != r.Parent {
				continue
			}
			if e = a.Store.Commit(ctx, r); e != nil {
				return e
			}
			delete(pending, id)
			advanced = true
		}
		if !advanced {
			return errors.New("snapshot history has a gap or fork")
		}
	}
	slog.Info("history recovery completed", "recovered_revisions", recovered)
	return nil
}
