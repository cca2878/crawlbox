package main

import (
	"context"
	"example.org/crawler/manager/internal/app"
	"example.org/crawler/manager/internal/catalog"
	"example.org/crawler/manager/internal/config"
	"example.org/crawler/manager/internal/kopia"
	"example.org/crawler/manager/internal/kopiaui"
	rt "example.org/crawler/manager/internal/runtime"
	"example.org/crawler/manager/internal/web"
	"flag"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	level := slog.LevelInfo
	if value := os.Getenv("CRAWLBOX_LOG_LEVEL"); value != "" {
		if err := level.UnmarshalText([]byte(value)); err != nil {
			fmt.Fprintln(os.Stderr, "invalid CRAWLBOX_LOG_LEVEL (use debug, info, warn or error)")
			os.Exit(1)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	if e := run(); e != nil {
		slog.Error("manager stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	defer rt.CloseCache(context.Background())
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		path := "/data/admin.yaml"
		if len(os.Args) > 3 {
			return fmt.Errorf("usage: manager reset-password [credentials-file]")
		}
		if len(os.Args) == 3 {
			path = os.Args[2]
		}
		if err := resetPassword(path, os.Stdin); err != nil {
			return err
		}
		fmt.Println("Administrator password updated. Restart the manager to apply it.")
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "serve-auto" {
		data := os.Getenv("CRAWLBOX_DATA")
		if data == "" {
			data = "/data"
		}
		return runServer(filepath.Join(data, "config.yaml"), true)
	}
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		b, e := io.ReadAll(io.LimitReader(os.Stdin, 74))
		if e != nil {
			return e
		}
		h, e := bcrypt.GenerateFromPassword([]byte(strings.TrimSuffix(string(b), "\n")), bcrypt.DefaultCost)
		if e != nil {
			return e
		}
		fmt.Println(string(h))
		return nil
	}
	path := flag.String("config", "config.yaml", "configuration file")
	flag.Parse()
	return runServer(*path, false)
}
func runServer(path string, firstSetup bool) error {
	slog.Info("loading manager configuration", "path", path)
	c, e := config.Load(path)
	if e != nil {
		return e
	}

	if e = os.MkdirAll(c.DataDir, 0700); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(c.DataDir, "manager.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("data directory in use: %w", e)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if firstSetup {
		if e = ensureAdministrator(ctx, c.DataDir); e != nil {
			return e
		}
	}
	creds, e := web.LoadCredentials(c.Credentials)
	if e != nil {
		return e
	}
	store, e := catalog.Open(filepath.Join(c.DataDir, "catalog.sqlite"))
	if e != nil {
		return e
	}
	defer store.Close()
	slog.Info("initializing sources and recovering history", "sources", len(c.Sources))
	a, e := app.New(ctx, c, store, &kopia.CLI{Binary: c.KopiaBinary, Config: c.KopiaConfig})
	if e != nil {
		return e
	}
	defer a.Close()
	if e = a.Start(); e != nil {
		return e
	}

	// Container defaults are independent of the business Kopia CLI connection.
	pc := c.KopiaUIProxy
	if pc.Listen == "" {
		pc.Listen = os.Getenv("KOPIA_UI_PROXY_LISTEN")
	}
	if pc.Target == "" {
		pc.Target = os.Getenv("KOPIA_UI_PROXY_TARGET")
	}
	if pc.FingerprintFile == "" {
		pc.FingerprintFile = os.Getenv("KOPIA_UI_PROXY_PIN_FILE")
	}
	var proxy *kopiaui.Proxy
	if pc.Listen != "" {
		proxy, e = kopiaui.New(ctx, pc, store)
		if e != nil {
			return e
		}
		defer proxy.Close()
		listener, err := net.Listen("tcp", pc.Listen)
		if err != nil {
			return fmt.Errorf("listen for Kopia UI proxy: %w", err)
		}
		proxyServer := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
		defer proxyServer.Close()
		go func() {
			if err := proxyServer.Serve(listener); err != nil && err != http.ErrServerClosed {
				slog.Error("Kopia UI proxy listener stopped")
				stop()
			}
		}()
		slog.Info("Kopia UI proxy listening", "address", pc.Listen, "enabled", proxy.Enabled())
	}
	srv := &http.Server{Addr: c.Listen, Handler: (&web.Server{App: a, Credentials: creds, KopiaUIProxy: proxy}).Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("manager listening", "address", c.Listen, "sources", len(c.Sources))
	if len(c.Sources) == 0 {
		slog.Info("no sources configured; waiting for source configuration")
	}
	e = srv.ListenAndServe()
	if e == http.ErrServerClosed {
		slog.Info("manager shutdown completed")
		return nil
	}
	return e
}
