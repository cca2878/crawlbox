// Package kopiaui provides a separate, transparent public path to the Kopia UI.
package kopiaui

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"

	"example.org/crawler/manager/internal/catalog"
	"example.org/crawler/manager/internal/config"
)

const setting = "kopia_ui_proxy_enabled"

type Proxy struct {
	store     *catalog.Store
	enabled   atomic.Bool
	mu        sync.Mutex
	reverse   *httputil.ReverseProxy
	transport *http.Transport
}

func New(ctx context.Context, c config.KopiaUIProxy, store *catalog.Store) (*Proxy, error) {
	target, err := url.Parse(c.Target)
	if err != nil || target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return nil, errors.New("Kopia UI proxy target must be an HTTP(S) origin without credentials")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Use the configured container-network endpoint directly.
	if c.FingerprintFile != "" {
		if target.Scheme != "https" {
			return nil, errors.New("Kopia UI certificate pin requires HTTPS")
		}
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The repository server uses a self-signed certificate, authenticated by its
			// separately supplied SHA-256 pin. Never accept an unverified certificate.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				data, err := os.ReadFile(c.FingerprintFile)
				if err != nil {
					return errors.New("Kopia UI certificate pin is unavailable")
				}
				var handoff struct {
					Fingerprint string `json:"fingerprint"`
				}
				if json.Unmarshal(data, &handoff) != nil {
					return errors.New("invalid Kopia UI certificate pin")
				}
				pin, err := hex.DecodeString(handoff.Fingerprint)
				if err != nil || len(pin) != sha256.Size || len(cs.PeerCertificates) == 0 {
					return errors.New("invalid Kopia UI certificate pin")
				}
				actual := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if actual != [sha256.Size]byte(pin) {
					return errors.New("Kopia UI certificate pin mismatch")
				}
				return nil
			},
		}
	}
	p := &Proxy{store: store, transport: transport}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Do not log URLs, headers or upstream error text containing user data.
			slog.Warn("Kopia UI proxy upstream request failed")
			http.Error(w, "Kopia UI is unavailable", http.StatusBadGateway)
		},
	}
	enabled, err := store.BoolSetting(ctx, setting)
	if err != nil {
		return nil, fmt.Errorf("read Kopia UI proxy setting: %w", err)
	}
	p.enabled.Store(enabled)
	return p, nil
}

func (p *Proxy) Enabled() bool { return p.enabled.Load() }
func (p *Proxy) SetEnabled(ctx context.Context, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.store.SetBoolSetting(ctx, setting, enabled); err != nil {
		return err
	}
	p.enabled.Store(enabled)
	if !enabled {
		p.transport.CloseIdleConnections()
	}
	slog.Info("Kopia UI proxy setting changed", "enabled", enabled)
	return nil
}
func (p *Proxy) Close() { p.transport.CloseIdleConnections() }
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.Enabled() {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Kopia UI proxy is disabled", http.StatusServiceUnavailable)
		return
	}
	p.reverse.ServeHTTP(w, r)
}
