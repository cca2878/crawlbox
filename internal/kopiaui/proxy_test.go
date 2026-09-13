package kopiaui_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.org/crawler/manager/internal/app"
	"example.org/crawler/manager/internal/catalog"
	"example.org/crawler/manager/internal/config"
	"example.org/crawler/manager/internal/kopiaui"
	"example.org/crawler/manager/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func TestProxyIsolationToggleAndPersistence(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if user, password, ok := r.BasicAuth(); !ok || user != "kopia" || password != "upstream-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="kopia"`)
			w.WriteHeader(401)
			return
		}
		if r.Host != "nas.example:8081" || r.Header.Get("Origin") != "http://nas.example:8081" {
			t.Error("external host/origin changed")
		}
		if r.URL.RawQuery != "download=1" || r.Header.Get("Cookie") != "test=value" {
			t.Error("query/cookie changed")
		}
		http.ServeContent(w, r, "fixture.txt", time.Time{}, strings.NewReader("abcdefgh"))
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "management.sqlite")
	store, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.KopiaUIProxy{Target: upstream.URL}
	p, err := kopiaui.New(context.Background(), cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	request := func(auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://nas.example:8081/file?download=1", nil)
		if auth {
			r.SetBasicAuth("kopia", "upstream-password")
		}
		r.Header.Set("Origin", "http://nas.example:8081")
		r.Header.Set("Cookie", "test=value")
		r.Header.Set("Range", "bytes=2-4")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	if w := request(false); w.Code != 503 || calls.Load() != 0 {
		t.Fatal("disabled proxy reached upstream")
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("manager-password"), bcrypt.MinCost)
	control := (&web.Server{App: &app.App{Store: store}, Credentials: web.Credentials{Username: "manager", PasswordHash: string(hash)}, KopiaUIProxy: p}).Handler()
	toggle := func(auth, origin, value string) int {
		r := httptest.NewRequest("POST", "http://nas.example:8080/ui/kopia-ui-proxy", strings.NewReader("enabled="+value))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if auth == "manager" {
			r.SetBasicAuth("manager", "manager-password")
		} else {
			r.Header.Set("Authorization", auth)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		control.ServeHTTP(w, r)
		return w.Code
	}
	if toggle("Bearer test", "", "true") != 401 || toggle("manager", "http://evil.example", "true") != 403 || p.Enabled() {
		t.Fatal("control authentication or CSRF bypass")
	}
	if toggle("manager", "", "true") != 303 {
		t.Fatal("enable failed")
	}
	if w := request(false); w.Code != 401 || w.Header().Get("WWW-Authenticate") != `Basic realm="kopia"` {
		t.Fatal("upstream authentication not preserved")
	}
	if w := request(true); w.Code != 206 || w.Body.String() != "cde" || w.Header().Get("Content-Range") != "bytes 2-4/8" {
		t.Fatal("range response not preserved")
	}
	restoredStore, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	restored, err := kopiaui.New(context.Background(), cfg, restoredStore)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !restored.Enabled() {
		t.Fatal("enabled setting was not persisted")
	}
	// A failed settings write must not change the live gate.
	if err = restoredStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err = restored.SetEnabled(context.Background(), false); err == nil || !restored.Enabled() {
		t.Fatal("failed persistence changed live setting")
	}
	if toggle("manager", "", "false") != 303 {
		t.Fatal("disable failed")
	}
	before := calls.Load()
	if w := request(true); w.Code != 503 || calls.Load() != before {
		t.Fatal("disabled proxy forwarded request")
	}
	if err = store.Register(context.Background(), map[string]string{"fixture": "fixture"}); err != nil {
		t.Fatal(err)
	}
	_, token, err := store.CreateToken(context.Background(), "test", []string{"fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/kopia-ui-proxy", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	control.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("public API exposes proxy control")
	}
}

func TestProxyCertificatePin(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "upstream") }))
	defer upstream.Close()
	store, err := catalog.Open(filepath.Join(t.TempDir(), "management.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pinPath := filepath.Join(t.TempDir(), "connection.json")
	sum := sha256.Sum256(upstream.Certificate().Raw)
	for _, test := range []struct {
		name, pin string
		status    int
	}{
		{"matching", hex.EncodeToString(sum[:]), 200},
		{"mismatch", strings.Repeat("0", 64), 502},
		{"malformed", "bad", 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(pinPath, []byte(fmt.Sprintf(`{"fingerprint":%q}`, test.pin)), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := kopiaui.New(context.Background(), config.KopiaUIProxy{Target: upstream.URL, FingerprintFile: pinPath}, store)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err = p.SetEnabled(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(p)
			defer server.Close()
			response, err := http.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			io.Copy(io.Discard, response.Body)
			if response.StatusCode != test.status {
				t.Fatal(response.StatusCode)
			}
		})
	}
}
