package main

import (
	"context"
	"example.org/crawler/manager/internal/app"
	"example.org/crawler/manager/internal/catalog"
	"example.org/crawler/manager/internal/config"
	"example.org/crawler/manager/internal/kopia"
	rt "example.org/crawler/manager/internal/runtime"
	"example.org/crawler/manager/internal/web"
	"flag"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		slog.Error("manager stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	defer rt.CloseCache(context.Background())
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
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	creds, e := web.LoadCredentials(c.Credentials)
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
	store, e := catalog.Open(filepath.Join(c.DataDir, "catalog.sqlite"))
	if e != nil {
		return e
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a, e := app.New(ctx, c, store, &kopia.CLI{Binary: c.KopiaBinary, Config: c.KopiaConfig})
	if e != nil {
		return e
	}
	defer a.Close()
	if e = a.Start(); e != nil {
		return e
	}
	srv := &http.Server{Addr: c.Listen, Handler: (&web.Server{App: a, Credentials: creds}).Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("manager listening", "address", c.Listen)
	e = srv.ListenAndServe()
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
