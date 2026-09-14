package web

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/cca2878/crawlbox/internal/app"
	"github.com/cca2878/crawlbox/internal/catalog"
	"github.com/cca2878/crawlbox/internal/kopiaui"
	"github.com/cca2878/crawlbox/internal/model"
	rt "github.com/cca2878/crawlbox/internal/runtime"
	"github.com/cca2878/crawlbox/internal/wire"
	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Credentials struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}
type Server struct {
	KopiaUIProxy *kopiaui.Proxy
	App          *app.App
	Credentials  Credentials
	mu           sync.Mutex
}
type authKey struct{}

func LoadCredentials(path string) (Credentials, error) {
	var c Credentials
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if e = yaml.Unmarshal(b, &c); e != nil {
		return c, e
	}
	if c.Username == "" {
		return c, errors.New("username required")
	}
	_, e = bcrypt.Cost([]byte(c.PasswordHash))
	return c, e
}
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			next.ServeHTTP(w, r)
		})
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.bearer)
		r.Get("/sources", s.sources)
		r.Route("/sources/{source}", func(r chi.Router) {
			r.Use(s.scope)
			r.Get("/revisions", s.revisions)
			r.Get("/tags", s.tags)
			r.Route("/revisions/{revision}", func(r chi.Router) {
				r.Get("/", s.metadata)
				r.Get("/metadata", s.metadata)
				r.Get("/changes", s.changes)
				r.Get("/files", s.files)
				r.Get("/files/*", s.file)
				r.Head("/files/*", s.file)
				r.Get("/artifacts", s.artifacts)
				r.Get("/artifacts/*", s.artifact)
				r.Head("/artifacts/*", s.artifact)
				r.Get("/archive", s.archive)
			})
		})
	})
	r.Route("/ui", func(r chi.Router) {
		r.Use(s.basic)
		r.Use(http.NewCrossOriginProtection().Handler)
		r.Get("/", s.ui)
		r.Post("/sources/{source}/trigger", s.trigger)
		r.Post("/runs/{run}/cancel", s.cancelRun)
		r.Post("/tokens", s.createToken)
		r.Post("/kopia-ui-proxy", s.toggleKopiaUIProxy)
		r.Post("/tokens/{token}/revoke", s.revoke)
	})
	return r
}
func (s *Server) bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.Fields(r.Header.Get("Authorization"))
		if len(p) != 2 || !strings.EqualFold(p[0], "Bearer") {
			unauthorized(w, "Bearer")
			return
		}
		t, e := s.App.Store.Authenticate(r.Context(), p[1])
		if e != nil {
			unauthorized(w, "Bearer")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, t)))
	})
}
func unauthorized(w http.ResponseWriter, scheme string) {
	w.Header().Set("WWW-Authenticate", scheme+` realm="manager"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
func (s *Server) scope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := r.Context().Value(authKey{}).(catalog.Token)
		if !slices.Contains(t.Sources, chi.URLParam(r, "source")) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) basic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		valid := false
		if ok && len(p) <= 72 {
			valid = bcrypt.CompareHashAndPassword([]byte(s.Credentials.PasswordHash), []byte(p)) == nil
		}
		if !valid || subtle.ConstantTimeCompare([]byte(u), []byte(s.Credentials.Username)) != 1 {
			unauthorized(w, "Basic")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter) { http.Error(w, "request failed", http.StatusInternalServerError) }
func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	t := r.Context().Value(authKey{}).(catalog.Token)
	out := []map[string]string{}
	for _, id := range t.Sources {
		if _, e := s.App.Store.Revision(r.Context(), id, "latest"); e == nil {
			name := id
			if v, ok := s.App.Sources[id]; ok && v.Config.Name != "" {
				name = v.Config.Name
			}
			out = append(out, map[string]string{"id": id, "name": name})
		}
	}
	respond(w, out)
}
func (s *Server) revisions(w http.ResponseWriter, r *http.Request) {
	list, e := s.App.Store.Revisions(r.Context(), chi.URLParam(r, "source"))
	if e != nil {
		failure(w)
		return
	}
	out := []map[string]any{}
	for _, v := range list {
		out = append(out, publicRevision(v))
	}
	respond(w, out)
}
func publicRevision(v model.Revision) map[string]any {
	return map[string]any{"id": v.ID, "source": v.Source, "parent": v.Parent, "created_at": v.CreatedAt, "metadata": v.Metadata, "tags": v.Tags, "plugin": map[string]string{"id": v.Plugin.ID, "version": v.Plugin.Version}}
}
func (s *Server) revision(w http.ResponseWriter, r *http.Request) (model.Revision, bool) {
	v, e := s.App.Store.Revision(r.Context(), chi.URLParam(r, "source"), chi.URLParam(r, "revision"))
	if e != nil {
		http.NotFound(w, r)
		return v, false
	}
	return v, true
}
func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.revision(w, r); ok {
		respond(w, publicRevision(v))
	}
}
func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.revision(w, r); ok {
		respond(w, map[string]any{"files": v.Changes, "artifacts": v.ArtifactChanges})
	}
}
func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	list, e := s.App.Store.Revisions(r.Context(), chi.URLParam(r, "source"))
	if e != nil {
		failure(w)
		return
	}
	out := []map[string]string{}
	for _, v := range list {
		for _, tag := range v.Tags {
			out = append(out, map[string]string{"tag": tag, "revision": v.ID})
		}
	}
	respond(w, out)
}
func page(w http.ResponseWriter, r *http.Request, entries []wire.Entry) {
	off := 0
	lim := 1000
	var e error
	if v := r.URL.Query().Get("offset"); v != "" {
		off, e = strconv.Atoi(v)
		if e != nil {
			http.Error(w, "invalid offset", 400)
			return
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		lim, e = strconv.Atoi(v)
		if e != nil {
			http.Error(w, "invalid limit", 400)
			return
		}
	}
	if off < 0 || lim < 1 || lim > 1000 {
		http.Error(w, "invalid pagination", 400)
		return
	}
	if off > len(entries) {
		off = len(entries)
	}
	end := min(off+lim, len(entries))
	respond(w, map[string]any{"entries": entries[off:end], "total": len(entries), "next_offset": end})
}
func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.revision(w, r); ok {
		page(w, r, v.Files)
	}
}
func (s *Server) artifacts(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.revision(w, r); ok {
		page(w, r, v.Artifacts)
	}
}
func (s *Server) file(w http.ResponseWriter, r *http.Request)     { s.serveFile(w, r, "files") }
func (s *Server) artifact(w http.ResponseWriter, r *http.Request) { s.serveFile(w, r, "artifacts") }
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, kind string) {
	v, ok := s.revision(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "*")
	if !rt.SafePath(name) {
		http.NotFound(w, r)
		return
	}
	list := v.Files
	if kind == "artifacts" {
		list = v.Artifacts
	}
	var entry *wire.Entry
	for i := range list {
		if list[i].Path == name {
			entry = &list[i]
			break
		}
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.trimCache("")
	dir := filepath.Join(s.App.Config.DataDir, "cache", v.ID, kind)
	p := filepath.Join(dir, filepath.FromSlash(name))
	e := os.MkdirAll(filepath.Dir(p), 0700)
	if e == nil {
		got, err := rt.FileEntry(p, name)
		if err != nil || got != *entry {
			_ = os.Remove(p)
			e = s.App.Backend.Restore(r.Context(), v.Snapshot, kind+"/"+name, p)
			if e == nil {
				got, e = rt.FileEntry(p, name)
				if e == nil && got != *entry {
					e = errors.New("historical digest mismatch")
				}
			}
		}
	}
	var f *os.File
	if e == nil {
		f, e = os.Open(p)
	}
	s.mu.Unlock()
	if e != nil {
		failure(w)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("ETag", `"`+entry.SHA256+`"`)
	http.ServeContent(w, r, filepath.Base(name), v.CreatedAt, f)
}
func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	v, ok := s.revision(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.trimCache("")
	s.trimCache("")
	root := filepath.Join(s.App.Config.DataDir, "cache", v.ID)
	if e := os.MkdirAll(root, 0700); e != nil {
		failure(w)
		return
	}
	p := filepath.Join(root, "archive.tar.gz")
	if f, e := os.Open(p); e == nil {
		defer f.Close()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+v.ID+`.tar.gz"`)
		http.ServeContent(w, r, v.ID+".tar.gz", v.CreatedAt, f)
		return
	}
	tmp, e := os.MkdirTemp(root, "archive-")
	if e != nil {
		failure(w)
		return
	}
	defer os.RemoveAll(tmp)
	tree := filepath.Join(tmp, "tree")
	if e = s.App.Backend.Restore(r.Context(), v.Snapshot, "", tree); e != nil {
		failure(w)
		return
	}
	f, e := os.Create(filepath.Join(tmp, "archive.tar.gz"))
	if e != nil {
		failure(w)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+v.ID+`.tar.gz"`)
	gz := gzip.NewWriter(io.MultiWriter(w, f))
	tw := tar.NewWriter(gz)
	for kind, entries := range map[string][]wire.Entry{"files": v.Files, "artifacts": v.Artifacts} {
		for _, entry := range entries {
			if r.Context().Err() != nil {
				return
			}
			if !rt.SafePath(entry.Path) {
				return
			}
			fp := filepath.Join(tree, kind, filepath.FromSlash(entry.Path))
			got, e := rt.FileEntry(fp, entry.Path)
			if e != nil || got != entry {
				return
			}
			if e = tw.WriteHeader(&tar.Header{Name: kind + "/" + entry.Path, Mode: 0600, Size: entry.Size, ModTime: v.CreatedAt}); e != nil {
				return
			}
			in, e := os.Open(fp)
			if e != nil {
				return
			}
			_, e = io.Copy(tw, in)
			in.Close()
			if e != nil {
				return
			}
		}
	}
	if e = tw.Close(); e != nil {
		return
	}
	if e = gz.Close(); e != nil {
		return
	}
	if e = f.Close(); e != nil {
		return
	}
	_ = os.Rename(filepath.Join(tmp, "archive.tar.gz"), p)
}

//go:embed ui.html
var uiHTML string
var uiTemplate = template.Must(template.New("ui").Parse(uiHTML))

// Terminal results take precedence over the last progress event, including
// for historical records written before this presentation rule existed.
func runMessage(run model.Run) string {
	switch run.Status {
	case "succeeded":
		return "采集成功，已发布新版本。"
	case "no_change":
		return "检查完成，无变化。"
	case "failed":
		return "任务失败，请查看错误详情。"
	case "interrupted":
		return "任务已中断。"
	default:
		return run.Progress
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, secret string) {
	page := r.URL.Query().Get("page")
	switch page {
	case "runs", "revisions", "tokens", "settings":
	default:
		page = "sources"
	}
	if secret != "" {
		page = "tokens"
	}
	type src struct {
		ID, Next, Revision, Status, Progress, Error string
		Active                                      bool
	}
	sources := []src{}
	history := []model.Revision{}
	for id := range s.App.Sources {
		next := "manual"
		if n := s.App.Next(id); !n.IsZero() {
			next = n.UTC().Format(time.RFC3339)
		}
		v, _ := s.App.Store.Revision(r.Context(), id, "latest")
		if revisions, e := s.App.Store.Revisions(r.Context(), id); e == nil {
			history = append(history, revisions[:min(20, len(revisions))]...)
		}
		sources = append(sources, src{ID: id, Next: next, Revision: v.ID})
	}
	slices.SortFunc(sources, func(a, b src) int { return strings.Compare(a.ID, b.ID) })
	var runs []model.Run
	var e error
	pagination := struct{ Number, Pages, Total, Previous, Next int }{}
	if page == "runs" {
		const pageSize = 20
		pagination.Total, e = s.App.Store.RunCount(r.Context())
		if e != nil {
			failure(w)
			return
		}
		pagination.Pages = max(1, (pagination.Total+pageSize-1)/pageSize)
		number, err := strconv.Atoi(r.URL.Query().Get("runs_page"))
		if err != nil {
			number = 1
		}
		pagination.Number = min(max(number, 1), pagination.Pages)
		if pagination.Number > 1 {
			pagination.Previous = pagination.Number - 1
		}
		if pagination.Number < pagination.Pages {
			pagination.Next = pagination.Number + 1
		}
		runs, e = s.App.Store.RunsPage(r.Context(), pageSize, (pagination.Number-1)*pageSize)
	} else {
		runs, e = s.App.Store.Runs(r.Context())
	}
	if e != nil {
		failure(w)
		return
	}
	// Runs are newest first. Show the latest available record for each source.
	for i := range sources {
		for _, run := range runs {
			if run.Source == sources[i].ID {
				sources[i].Status = run.Status
				sources[i].Progress = runMessage(run)
				sources[i].Error = run.Error
				switch run.Status {
				case "queued", "running", "validating", "snapshotting", "committing":
					sources[i].Active = true
				}
				break
			}
		}
	}
	slices.SortFunc(history, func(a, b model.Revision) int { return b.CreatedAt.Compare(a.CreatedAt) })
	type runView struct {
		model.Run
		CanCancel bool
		Stopping  bool
		Message   string
	}
	runViews := make([]runView, 0, len(runs))
	for _, run := range runs {
		runViews = append(runViews, runView{Run: run, Message: runMessage(run), CanCancel: s.App.CanCancel(run.ID), Stopping: s.App.CancellationRequested(run.ID)})
	}
	tokens, e := s.App.Store.Tokens(r.Context())
	if e != nil {
		failure(w)
		return
	}
	type tokenView struct {
		catalog.Token
		Status    string
		DisplayID string
	}
	views := []tokenView{}
	for _, t := range tokens {
		status := "active"
		if t.Revoked {
			status = "revoked"
		} else if t.Expires != nil && !time.Now().Before(*t.Expires) {
			status = "expired"
		}
		displayID := "***.***"
		if len(t.ID) > 6 {
			displayID = t.ID[:3] + "***" + t.ID[len(t.ID)-3:] + ".***"
		}
		views = append(views, tokenView{t, status, displayID})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = uiTemplate.Execute(w, map[string]any{"Page": page, "KopiaUIProxy": s.KopiaUIProxy, "Secret": secret, "Sources": sources, "History": history, "Runs": runViews, "RunPagination": pagination, "Tokens": views})
}
func (s *Server) ui(w http.ResponseWriter, r *http.Request) { s.render(w, r, "") }
func (s *Server) trigger(w http.ResponseWriter, r *http.Request) {
	_, e := s.App.Trigger(chi.URLParam(r, "source"))
	if e != nil {
		http.Error(w, "source unavailable", 409)
		return
	}
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if e := r.ParseForm(); e != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	var expiry *time.Time
	if raw := r.PostForm.Get("expires"); raw != "" {
		t, e := time.Parse("2006-01-02T15:04", raw)
		if e != nil {
			http.Error(w, "invalid UTC expiry", 400)
			return
		}
		expiry = &t
	}
	sources := slices.Compact(r.PostForm["sources"])
	_, secret, e := s.App.Store.CreateToken(r.Context(), r.PostForm.Get("name"), sources, expiry)
	if e != nil {
		http.Error(w, "invalid name, sources or expiry", 400)
		return
	}
	s.render(w, r, secret)
}
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if e := s.App.Store.Revoke(r.Context(), chi.URLParam(r, "token")); e != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/ui/?page=tokens", http.StatusSeeOther)
}

// trimCache runs under the cache lock. Removing an opened file does not invalidate
// an in-flight Linux download; authorization has already been checked per request.
func (s *Server) trimCache(keep string) {
	root := filepath.Join(s.App.Config.DataDir, "cache")
	dirs, e := os.ReadDir(root)
	if e != nil {
		return
	}
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, d.Name())
		var size int64
		_ = filepath.WalkDir(p, func(_ string, de os.DirEntry, e error) error {
			if e != nil {
				return nil
			}
			if de.Type().IsRegular() {
				if st, e := de.Info(); e == nil {
					size += st.Size()
				}
			}
			return nil
		})
		st, e := d.Info()
		if e != nil {
			continue
		}
		items = append(items, item{p, size, st.ModTime()})
		total += size
	}
	slices.SortFunc(items, func(a, b item) int { return a.mod.Compare(b.mod) })
	for _, it := range items {
		if total <= s.App.Config.CacheBytes {
			break
		}
		if it.path == keep {
			continue
		}
		if os.RemoveAll(it.path) == nil {
			total -= it.size
		}
	}
}

func (s *Server) toggleKopiaUIProxy(w http.ResponseWriter, r *http.Request) {
	if s.KopiaUIProxy == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	value := r.PostForm.Get("enabled")
	if value != "true" && value != "false" {
		http.Error(w, "invalid enabled value", 400)
		return
	}
	if err := s.KopiaUIProxy.SetEnabled(r.Context(), value == "true"); err != nil {
		failure(w)
		return
	}
	http.Redirect(w, r, "/ui/?page=settings", http.StatusSeeOther)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	err := s.App.Cancel(chi.URLParam(r, "run"))
	if errors.Is(err, app.ErrRunNotActive) {
		http.Error(w, "run is no longer active", http.StatusConflict)
		return
	}
	if errors.Is(err, app.ErrRunSealed) {
		http.Error(w, "run is snapshotting, committing or finishing; cannot cancel", http.StatusConflict)
		return
	}
	if err != nil {
		failure(w)
		return
	}
	http.Redirect(w, r, "/ui/?page=runs", http.StatusSeeOther)
}
