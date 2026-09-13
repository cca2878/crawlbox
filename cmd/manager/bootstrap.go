package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"example.org/crawler/manager/internal/bootstrap"
	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

func managed(mode string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	data := envDefault("CRAWLBOX_DATA", "/data")
	shared := envDefault("CRAWLBOX_BOOTSTRAP", "/bootstrap")
	lock, err := bootstrap.Lock(data)
	if err != nil {
		return err
	}
	defer lock.Close()
	options := bootstrap.Options{Binary: envDefault("KOPIA_BINARY", "kopia"), Data: data, Shared: shared}
	if mode == "kopia-server" {
		options.URL = envDefault("KOPIA_LISTEN", "https://0.0.0.0:51515")
		initCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cmd, err := options.Server(initCtx)
		cancel()
		if err != nil {
			return err
		}
		// Keep the server bootstrap lock across exec, preventing concurrent initialization.
		if _, err = unix.FcntlInt(lock.Fd(), unix.F_SETFD, 0); err != nil {
			return err
		}
		// Execute the server as PID 1 so it receives container stop signals directly.
		return syscall.Exec(cmd.Path, cmd.Args, cmd.Env)
	}
	if err := ensureAdministrator(ctx, data); err != nil {
		return err
	}
	cfg, err := prepareConfig(data)
	if err != nil {
		return err
	}
	options.URL = envDefault("KOPIA_URL", "https://kopia:51515")
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	slog.Info("waiting for internal Kopia connection")
	err = options.Connect(connectCtx)
	cancel()
	if err != nil {
		return err
	}
	// The bootstrap lock is released when exec replaces this process. The normal
	// manager acquires its own data lock before opening the catalog.
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(binary, []string{binary, "-config", cfg}, os.Environ())
}
func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func prepareConfig(data string) (string, error) {
	credentials := filepath.Join(data, "admin.yaml")
	if _, err := os.Stat(credentials); err != nil {
		return "", err
	}
	cfg := filepath.Join(data, "config.yaml")
	if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		b, err := yaml.Marshal(map[string]any{"listen": ":8080", "data_dir": data, "credentials": credentials, "kopia_binary": envDefault("KOPIA_BINARY", "kopia"), "kopia_config": filepath.Join(data, "connection", "repository.config"), "parallel": 1, "cache_bytes": int64(20 << 30), "sources": []any{}})
		if err != nil {
			return "", err
		}
		if err = bootstrap.AtomicWrite(cfg, b); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if _, err := os.Stat(credentials); err != nil {
		return "", fmt.Errorf("administrator credentials: %w", err)
	}
	return cfg, nil
}
