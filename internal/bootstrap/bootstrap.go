// Package bootstrap initializes the self-contained two-container deployment.
package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Options struct {
	Binary, Data, Shared, URL string
}
type secrets struct {
	Repository string `json:"repository"`
	Worker     string `json:"worker"`
	Server     string `json:"server"`
}
type Connection struct {
	Password    string `json:"password"`
	Fingerprint string `json:"fingerprint"`
}

// AtomicWrite never leaves a partial credential or configuration file behind.
func AtomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".bootstrap-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func randomSecret() string { var b [32]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func (o Options) command(ctx context.Context, config, password string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, o.Binary, append([]string{"--config-file", config, "--disable-file-logging", "--no-progress"}, args...)...)
	cmd.Env = append(os.Environ(), "KOPIA_PASSWORD="+password, "KOPIA_CHECK_FOR_UPDATES=false")
	return cmd
}
func (o Options) run(ctx context.Context, config, password string, args ...string) error {
	// Do not include arguments, credentials or child output in errors/logs.
	if err := o.command(ctx, config, password, args...).Run(); err != nil {
		return fmt.Errorf("kopia %s failed: %w", args[0], err)
	}
	return nil
}
func Lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "bootstrap.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("bootstrap directory is already in use: %w", err)
	}
	return f, nil
}

// Server prepares the repository and returns the long-lived Kopia server command.
// The caller must hold Lock(Data) while preparing and running the server.
func (o Options) Server(ctx context.Context) (*exec.Cmd, error) {
	for _, p := range []string{o.Data, o.Shared} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, err
		}
	}
	secretPath := filepath.Join(o.Data, "secrets.json")
	var s secrets
	b, err := os.ReadFile(secretPath)
	if errors.Is(err, os.ErrNotExist) {
		slog.Info("creating storage credentials")
		// Refuse to invent new passwords for an existing repository.
		entries, e := os.ReadDir(filepath.Join(o.Data, "repository"))
		if e == nil && len(entries) > 0 {
			return nil, errors.New("repository exists but secrets.json is missing; restore its backup")
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		s = secrets{randomSecret(), randomSecret(), randomSecret()}
		b, _ = json.Marshal(s)
		err = AtomicWrite(secretPath, b)
	} else if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		return nil, err
	}
	if !validSecret(s.Repository) || !validSecret(s.Worker) || !validSecret(s.Server) {
		return nil, errors.New("invalid bootstrap secrets")
	}
	cfg := filepath.Join(o.Data, "repository.config")
	repo := filepath.Join(o.Data, "repository")
	if _, err = os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		verb := "create"
		entries, e := os.ReadDir(repo)
		if e == nil && len(entries) > 0 {
			verb = "connect"
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		slog.Info("initializing storage repository", "operation", verb)
		if err = o.run(ctx, cfg, s.Repository, "repository", verb, "filesystem", "--path", repo, "--cache-directory", filepath.Join(o.Data, "cache")); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err = o.run(ctx, cfg, s.Repository, "server", "users", "add", "worker@manager", "--user-password", s.Worker); err != nil {
		if err = o.run(ctx, cfg, s.Repository, "server", "users", "set", "worker@manager", "--user-password", s.Worker); err != nil {
			return nil, err
		}
	}
	cert := filepath.Join(o.Data, "server.crt")
	key := filepath.Join(o.Data, "server.key")
	// Use standard-library certificate generation; the client pins its SHA-256.
	if _, err = os.Stat(cert); errors.Is(err, os.ErrNotExist) {
		slog.Info("generating internal TLS certificate")
		if err = certificate(cert, key); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if _, err = tls.LoadX509KeyPair(cert, key); err != nil {
		return nil, err
	}
	b, err = os.ReadFile(cert)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("invalid server certificate")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(crt.Raw)
	b, _ = json.Marshal(Connection{s.Worker, hex.EncodeToString(digest[:])})
	if err = AtomicWrite(filepath.Join(o.Shared, "connection.json"), b); err != nil {
		return nil, err
	}
	cmd := o.command(ctx, cfg, s.Repository, "server", "start", "--address", o.URL, "--tls-cert-file", cert, "--tls-key-file", key, "--server-username=admin")
	cmd.Env = append(cmd.Env, "KOPIA_SERVER_PASSWORD="+s.Server)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// Connect retries until the server is ready; client cache and credentials are persistent.
func (o Options) Connect(ctx context.Context) error {
	cfg := filepath.Join(o.Data, "connection", "repository.config")
	if err := os.MkdirAll(filepath.Dir(cfg), 0700); err != nil {
		return err
	}
	attempts := 0
	for {
		attempts++
		if attempts == 1 || attempts%15 == 0 {
			slog.Info("waiting for Kopia readiness", "attempt", attempts)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for Kopia: %w", ctx.Err())
		}
		b, err := os.ReadFile(filepath.Join(o.Shared, "connection.json"))
		if err == nil {
			var c Connection
			if err = json.Unmarshal(b, &c); err != nil {
				return err
			}
			if len(c.Password) != 64 || len(c.Fingerprint) != 64 {
				return errors.New("invalid bootstrap connection")
			}
			attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = o.run(attempt, cfg, c.Password, "repository", "connect", "server", "--url", o.URL, "--server-cert-fingerprint", c.Fingerprint, "--override-username=worker", "--override-hostname=manager", "--cache-directory", filepath.Join(o.Data, "kopia-cache"), "--persist-credentials")
			cancel()
			if err == nil {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func certificate(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), DNSNames: []string{"kopia"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err = AtomicWrite(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})); err != nil {
		return err
	}
	return AtomicWrite(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func validSecret(s string) bool  { b, err := hex.DecodeString(s); return err == nil && len(b) == 32 }
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Initialize prepares a startup script consumed by the official Kopia image.
// It exposes only the worker connection to the manager's shared volume.
func (o Options) Initialize(ctx context.Context, managerData string) error {
	cmd, err := o.Server(ctx)
	if err != nil {
		return err
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -eu\n")
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "KOPIA_PASSWORD=") || strings.HasPrefix(entry, "KOPIA_SERVER_PASSWORD=") {
			key, value, _ := strings.Cut(entry, "=")
			script.WriteString("export " + key + "=" + shellQuote(value) + "\n")
		}
	}
	script.WriteString("exec /bin/kopia")
	for _, arg := range cmd.Args[1:] {
		script.WriteString(" " + shellQuote(arg))
	}
	script.WriteString("\n")
	if err = AtomicWrite(filepath.Join(o.Data, "start-server.sh"), []byte(script.String())); err != nil {
		return err
	}
	if managerData != "" {
		if err = os.MkdirAll(managerData, 0700); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			// Only adjust the mount roots and generated handoff; never walk the repository.
			for _, p := range []string{managerData, o.Shared, filepath.Join(o.Shared, "connection.json")} {
				if err = os.Chown(p, 10001, 10001); err != nil {
					return err
				}
			}
		}
	}
	slog.Info("storage initialization completed")
	return nil
}
