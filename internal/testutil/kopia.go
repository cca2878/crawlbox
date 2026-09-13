// Package testutil provisions an isolated authenticated Kopia server for integration tests.
package testutil

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"github.com/cca2878/crawlbox/internal/kopia"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func Kopia(t *testing.T) *kopia.CLI {
	t.Helper()
	binary := os.Getenv("KOPIA_BIN")
	if binary == "" {
		t.Skip("KOPIA_BIN required for real server integration")
	}
	binary, e := filepath.Abs(binary)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	serverConfig := filepath.Join(dir, "server.config")
	clientConfig := filepath.Join(dir, "client.config")
	env := append(os.Environ(), "KOPIA_PASSWORD=integration-only", "KOPIA_SERVER_PASSWORD=integration-only")
	run := func(cfg string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--config-file", cfg, "--disable-file-logging", "--no-progress"}, args...)...)
		cmd.Env = env
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("kopia %v: %v\n%s", args, e, b)
		}
		return b
	}
	run(serverConfig, "repository", "create", "filesystem", "--path", filepath.Join(dir, "repo"))
	run(serverConfig, "server", "user", "add", "worker@manager", "--user-password=integration-only")
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	addr := fmt.Sprintf("https://127.0.0.1:%d", port)
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	log, e := os.Create(filepath.Join(dir, "server.log"))
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(binary, "--config-file", serverConfig, "--disable-file-logging", "server", "start", "--address", addr, "--tls-generate-cert", "--tls-generate-rsa-key-size=2048", "--tls-cert-file", cert, "--tls-key-file", key, "--server-username=admin")
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() })
	ready := false
	for range 150 {
		c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if e == nil {
			c.Close()
			if b, e := os.ReadFile(cert); e == nil {
				if block, _ := pem.Decode(b); block != nil {
					ready = true
					break
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		b, _ := os.ReadFile(log.Name())
		t.Fatalf("server failed: %s", b)
	}
	b, e := os.ReadFile(cert)
	if e != nil {
		t.Fatal(e)
	}
	block, _ := pem.Decode(b)
	crt, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(crt.Raw)
	run(clientConfig, "repository", "connect", "server", "--url", addr, "--server-cert-fingerprint", hex.EncodeToString(h[:]), "--override-username=worker", "--override-hostname=manager")
	return &kopia.CLI{Binary: binary, Config: clientConfig}
}
