package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/fileutil"
	"github.com/cca2878/crawlbox/internal/wire"
	extism "github.com/extism/go-sdk"
	"github.com/gobwas/glob"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tetratelabs/wazero"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func SafePath(p string) bool {
	if p == "" || len(p) > 4096 || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00\r\n") {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." || s == "" {
			return false
		}
	}
	return true
}
func Sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func FileEntry(p, name string) (wire.Entry, error) {
	return FileEntryContext(context.Background(), p, name)
}
func FileEntryContext(ctx context.Context, p, name string) (wire.Entry, error) {
	if err := ctx.Err(); err != nil {
		return wire.Entry{}, err
	}
	f, e := os.Open(p)
	if e != nil {
		return wire.Entry{}, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, fileutil.ContextReader{Context: ctx, Reader: f})
	return wire.Entry{Path: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, e
}

type Object struct {
	Path   string
	Sealed bool
	// Entry carries the committed metadata of an object opened from the
	// previous revision. The catalog already holds that digest, so answering
	// stat from it avoids reading the bytes again.
	Entry *wire.Entry
}

// Previous describes the committed revision a run inherits from: where its
// bytes live, and what the catalog already recorded about them. Passing the
// committed entries keeps the host from re-deriving metadata the manager has
// stored, which for a large tree means reading every byte.
type Previous struct {
	Files           string
	Artifacts       string
	Entries         []wire.Entry
	ArtifactEntries []wire.Entry
}
type Candidate struct {
	Files     map[string]string
	Artifacts map[string]string
	Deletes   map[string]bool
	Result    wire.Result
}
type Host struct {
	Source            config.Source
	Descriptor        wire.Descriptor
	Dir               string
	Previous          string
	PreviousArtifacts string
	Progress          func(string)
	entries           []wire.Entry
	index             map[string]wire.Entry
	artifactIndex     map[string]wire.Entry
	mu                sync.Mutex
	objects           map[string]*Object
	used              int64
	candidate         Candidate
	client            *http.Client
	slots             chan struct{}
}

// Response header caps bound the JSON a hostile upstream can make the host
// build. They are not a confidentiality boundary.
const (
	maxResponseHeaders     = 64
	maxResponseHeaderBytes = 8 << 10
)

func NewHost(s config.Source, d wire.Descriptor, dir string, prev Previous, progress func(string)) *Host {
	h := &Host{Source: s, Descriptor: d, Dir: dir, Previous: prev.Files, PreviousArtifacts: prev.Artifacts, Progress: progress, objects: map[string]*Object{}, candidate: Candidate{Files: map[string]string{}, Artifacts: map[string]string{}, Deletes: map[string]bool{}}, slots: make(chan struct{}, s.DownloadParallel)}
	h.entries = append([]wire.Entry(nil), prev.Entries...)
	sort.Slice(h.entries, func(i, j int) bool { return h.entries[i].Path < h.entries[j].Path })
	h.index = index(prev.Entries)
	h.artifactIndex = index(prev.ArtifactEntries)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil {
			return nil, e
		}
		// Reaching inward requires an explicit grant in the source. A literal
		// address is not an exemption: writing the address instead of a name
		// that resolves to it is the same request, so treating one as safe
		// leaves the check trivially bypassable.
		if !h.Source.AllowPrivateTargets {
			for _, ip := range ips {
				if ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() {
					return nil, errors.New("private network target denied")
				}
			}
		}
		var last error
		for _, ip := range ips {
			c, e := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if e == nil {
				return c, nil
			}
			last = e
		}
		return nil, last
	}
	h.client = &http.Client{Transport: tr, Timeout: 10 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("redirect limit")
		}
		if e := h.allow(r.URL); e != nil {
			return e
		}
		if len(via) > 0 && via[0].URL.Host != r.URL.Host {
			r.Header.Del("Authorization")
			r.Header.Del("Cookie")
		}
		return nil
	}}
	return h
}
func index(entries []wire.Entry) map[string]wire.Entry {
	m := make(map[string]wire.Entry, len(entries))
	for _, e := range entries {
		m[e.Path] = e
	}
	return m
}
func (h *Host) Close() { h.client.CloseIdleConnections() }
func (h *Host) allow(u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" {
		return errors.New("invalid network target")
	}
	grant := false
	for _, v := range h.Source.Hosts {
		if strings.EqualFold(v, u.Host) {
			grant = true
		}
	}
	request := false
	for _, v := range h.Descriptor.Hosts {
		// '.' is a separator, so a wildcard covers one label rather than an
		// arbitrary depth: *.example.com requests api.example.com and not
		// a.b.example.com, keeping the requested surface as narrow as it reads.
		g, e := glob.Compile(v, '.')
		if e == nil && (g.Match(u.Host) || g.Match(u.Hostname())) {
			request = true
		}
	}
	if !grant || !request {
		return errors.New("network target not authorized")
	}
	return nil
}
func (h *Host) newObject() (string, *Object, error) {
	f, e := os.CreateTemp(h.Dir, "object-")
	if e != nil {
		return "", nil, e
	}
	if e = f.Close(); e != nil {
		return "", nil, e
	}
	id := filepath.Base(f.Name())
	o := &Object{Path: f.Name()}
	h.mu.Lock()
	h.objects[id] = o
	h.mu.Unlock()
	return id, o, nil
}
func (h *Host) Call(ctx context.Context, q wire.Request) (wire.Response, error) {
	var r wire.Response
	if e := ctx.Err(); e != nil {
		return r, e
	}
	if len(q.Data) > wire.Chunk || len(q.Body) > wire.Chunk {
		return r, errors.New("chunk limit")
	}
	switch q.Op {
	case "progress":
		if len(q.Data) > 4096 {
			return r, errors.New("progress limit")
		}
		if h.Progress != nil {
			h.Progress(string(q.Data))
		}
		return r, nil
	case "http":
		u, e := url.Parse(q.URL)
		if e != nil {
			return r, errors.New("invalid URL")
		}
		if e = h.allow(u); e != nil {
			return r, e
		}
		method := q.Method
		if method == "" {
			method = "GET"
		}
		if method != "GET" && method != "POST" && method != "HEAD" {
			return r, errors.New("unsupported HTTP method")
		}
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		case <-ctx.Done():
			return r, ctx.Err()
		}
		req, e := http.NewRequestWithContext(ctx, method, q.URL, bytes.NewReader(q.Body))
		if e != nil {
			return r, errors.New("invalid request")
		}
		for k, v := range q.Headers {
			if strings.EqualFold(k, "Host") {
				return r, errors.New("Host override denied")
			}
			req.Header.Set(k, v)
		}
		resp, e := h.client.Do(req)
		if e != nil {
			return r, errors.New("upstream request failed")
		}
		defer resp.Body.Close()
		id, o, e := h.newObject()
		if e != nil {
			return r, e
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		remaining := h.Source.StagingBytes - h.used
		if remaining < 0 {
			return r, errors.New("staging quota exceeded")
		}
		f, e := os.OpenFile(o.Path, os.O_WRONLY, 0600)
		if e != nil {
			return r, e
		}
		n, e := io.Copy(f, io.LimitReader(resp.Body, remaining+1))
		ce := f.Close()
		h.used += n
		if e != nil {
			return r, e
		}
		if ce != nil {
			return r, ce
		}
		if n > remaining {
			return r, errors.New("staging quota exceeded")
		}
		o.Sealed = true
		r.Handle = id
		r.Size = n
		r.Status = resp.StatusCode
		// Response headers reach the plugin in full. Selecting them by name
		// would make each new request style need a Manager change, which the
		// architecture rejects: a ranged request is ordinary HTTP, yet its
		// Content-Range is the only place a partial response states the full
		// size. Withholding headers protects nothing either, because the
		// plugin already reads the whole body, it chooses the request headers
		// itself, and the upstream never sees Manager state. The caps below
		// are resource protection, like the staging quota, and names are
		// ordered so truncation is deterministic.
		names := make([]string, 0, len(resp.Header))
		for k := range resp.Header {
			names = append(names, k)
		}
		sort.Strings(names)
		r.Headers = make(map[string]string, min(len(names), maxResponseHeaders))
		for _, k := range names {
			if len(r.Headers) >= maxResponseHeaders {
				break
			}
			v := resp.Header.Get(k)
			if len(v) > maxResponseHeaderBytes {
				v = v[:maxResponseHeaderBytes]
			}
			r.Headers[k] = v
		}
		return r, nil
	case "create":
		id, _, e := h.newObject()
		r.Handle = id
		return r, e
	case "previous_file", "previous_artifact":
		if !SafePath(q.Path) {
			return r, errors.New("invalid path")
		}
		root := h.Previous
		if q.Op == "previous_artifact" {
			root = h.PreviousArtifacts
		}
		if root == "" {
			return r, os.ErrNotExist
		}
		p := filepath.Join(root, filepath.FromSlash(q.Path))
		st, e := os.Lstat(p)
		if e != nil {
			return r, errors.New("previous object unavailable")
		}
		if !st.Mode().IsRegular() {
			return r, errors.New("not regular file")
		}
		known := h.index
		if q.Op == "previous_artifact" {
			known = h.artifactIndex
		}
		o := &Object{Path: p, Sealed: true}
		if committed, ok := known[q.Path]; ok && committed.Size == st.Size() {
			o.Entry = &committed
		}
		h.mu.Lock()
		id := fmt.Sprintf("previous-%d", len(h.objects))
		h.objects[id] = o
		h.mu.Unlock()
		r.Handle = id
		r.Size = st.Size()
		return r, nil
	case "list_files":
		if q.Offset < 0 || q.Limit < 0 || q.Limit > 1000 {
			return r, errors.New("invalid pagination")
		}
		limit := q.Limit
		if limit == 0 {
			limit = 1000
		}
		// The committed entries already describe this view, digests included.
		// Walking the tree to hash it again would read every byte to rebuild
		// what the catalog stored when the revision was published.
		r.Entries = []wire.Entry{}
		for i := q.Offset; i < int64(len(h.entries)) && len(r.Entries) < limit; i++ {
			r.Entries = append(r.Entries, h.entries[i])
		}
		return r, nil
	case "delete_file":
		if !SafePath(q.Path) {
			return r, errors.New("invalid path")
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.candidate.Files[q.Path]; ok {
			return r, errors.New("conflicting operation")
		}
		if h.candidate.Deletes[q.Path] {
			return r, errors.New("duplicate deletion")
		}
		h.candidate.Deletes[q.Path] = true
		return r, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	o, ok := h.objects[q.Handle]
	if !ok {
		return r, errors.New("unknown object")
	}
	switch q.Op {
	case "write":
		if o.Sealed {
			return r, errors.New("sealed object")
		}
		if h.used+int64(len(q.Data)) > h.Source.StagingBytes {
			return r, errors.New("staging quota exceeded")
		}
		f, e := os.OpenFile(o.Path, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return r, e
		}
		n, e := f.Write(q.Data)
		ce := f.Close()
		h.used += int64(n)
		if e != nil {
			return r, e
		}
		return r, ce
	case "close":
		o.Sealed = true
		return r, nil
	case "read":
		if q.Offset < 0 || q.Limit < 0 || q.Limit > wire.Chunk {
			return r, errors.New("invalid read")
		}
		lim := q.Limit
		if lim == 0 {
			lim = wire.Chunk
		}
		f, e := os.Open(o.Path)
		if e != nil {
			return r, e
		}
		defer f.Close()
		r.Data = make([]byte, lim)
		n, e := f.ReadAt(r.Data, q.Offset)
		r.Data = r.Data[:n]
		if e == io.EOF {
			e = nil
		}
		return r, e
	case "stat":
		if o.Entry != nil {
			r.Size, r.SHA256 = o.Entry.Size, o.Entry.SHA256
			return r, nil
		}
		v, e := FileEntryContext(ctx, o.Path, "")
		r.Size = v.Size
		r.SHA256 = v.SHA256
		return r, e
	case "publish_file", "publish_artifact":
		if !SafePath(q.Path) {
			return r, errors.New("invalid path")
		}
		if !o.Sealed {
			return r, errors.New("object must be sealed")
		}
		dst := h.candidate.Files
		if q.Op == "publish_artifact" {
			dst = h.candidate.Artifacts
		} else if h.candidate.Deletes[q.Path] {
			return r, errors.New("conflicting operation")
		}
		if _, ok := dst[q.Path]; ok {
			return r, errors.New("duplicate publication")
		}
		dst[q.Path] = o.Path
		return r, nil
	}
	return r, errors.New("unknown operation")
}

var compilationCache = wazero.NewCompilationCache()

func CloseCache(ctx context.Context) error { return compilationCache.Close(ctx) }
func invoke(ctx context.Context, s config.Source, wasm []byte, name string, input []byte, h *Host) ([]byte, error) {
	fn := extism.NewHostFunctionWithStack("call", func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
		var r wire.Response
		b, e := p.ReadBytes(stack[0])
		if e == nil && len(b) > 2*wire.Chunk {
			e = errors.New("host message limit")
		}
		var q wire.Request
		if e == nil {
			e = json.Unmarshal(b, &q)
		}
		if e == nil {
			if h == nil {
				e = errors.New("capabilities unavailable")
			} else {
				r, e = h.Call(ctx, q)
			}
		}
		if e != nil {
			r.Error = e.Error()
		}
		out, _ := json.Marshal(r)
		ptr, e := p.WriteBytes(out)
		if e != nil {
			panic(e)
		}
		stack[0] = ptr
	}, []extism.ValueType{extism.ValueTypePTR}, []extism.ValueType{extism.ValueTypePTR})
	p, e := extism.NewPlugin(ctx, extism.Manifest{Wasm: []extism.Wasm{extism.WasmData{Data: wasm}}, Memory: &extism.ManifestMemory{MaxPages: s.MemoryMB * 16, MaxHttpResponseBytes: 1, MaxVarBytes: int64(s.StateBytes)}, Timeout: uint64(s.Timeout.Milliseconds())}, extism.PluginConfig{EnableWasi: true, RuntimeConfig: wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithCompilationCache(compilationCache)}, []extism.HostFunction{fn})
	if e != nil {
		return nil, e
	}
	defer p.Close(context.Background())
	code, out, e := p.CallWithContext(ctx, name, input)
	if e != nil {
		return nil, e
	}
	if code != 0 {
		return nil, fmt.Errorf("plugin %s returned %d", name, code)
	}
	if len(out) > s.StateBytes+wire.Chunk {
		return nil, errors.New("plugin output exceeds limit")
	}
	return out, nil
}
func Load(ctx context.Context, s config.Source) (wire.Descriptor, []byte, error) {
	var d wire.Descriptor
	b, e := os.ReadFile(s.Plugin)
	if e != nil {
		return d, nil, e
	}
	if !strings.EqualFold(Sum(b), s.SHA256) {
		return d, nil, errors.New("plugin digest mismatch")
	}
	out, e := invoke(ctx, s, b, "describe", nil, nil)
	if e != nil {
		return d, nil, e
	}
	if e = json.Unmarshal(out, &d); e != nil {
		return d, nil, e
	}
	if d.ABI != wire.ABI || !config.ValidID(d.ID) || d.Version == "" {
		return d, nil, errors.New("unsupported plugin descriptor")
	}
	raw, e := json.Marshal(s.Config)
	if e != nil {
		return d, nil, e
	}
	if len(d.ConfigSchema) > 0 {
		var schema any
		if e = json.Unmarshal(d.ConfigSchema, &schema); e != nil {
			return d, nil, e
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.UseLoader(jsonschema.SchemeURLLoader{})
		if e = c.AddResource("schema.json", schema); e != nil {
			return d, nil, e
		}
		sc, e := c.Compile("schema.json")
		if e != nil {
			return d, nil, e
		}
		var v any
		if e = json.Unmarshal(raw, &v); e != nil {
			return d, nil, e
		}
		if e = sc.Validate(v); e != nil {
			return d, nil, e
		}
	}
	_, e = invoke(ctx, s, b, "validate_config", raw, nil)
	return d, b, e
}
func Execute(ctx context.Context, s config.Source, b []byte, in wire.Input, h *Host) (Candidate, error) {
	raw, e := json.Marshal(in)
	if e != nil {
		return Candidate{}, e
	}
	out, e := invoke(ctx, s, b, "run", raw, h)
	if e != nil {
		return Candidate{}, e
	}
	var result wire.Result
	if e = json.Unmarshal(out, &result); e != nil {
		return Candidate{}, e
	}
	if len(result.Message) > 4096 {
		return Candidate{}, errors.New("plugin result message exceeds 4096 bytes")
	}
	result.Message = strings.TrimSpace(result.Message)
	if result.Status != "candidate" && result.Status != "no_change" {
		return Candidate{}, errors.New("invalid plugin result")
	}
	if len(result.State) > s.StateBytes {
		return Candidate{}, errors.New("state limit")
	}
	if len(result.State) == 0 {
		result.State = json.RawMessage("null")
	}
	if len(result.Metadata) == 0 {
		result.Metadata = json.RawMessage("null")
	}
	h.candidate.Result = result
	return h.candidate, nil
}
