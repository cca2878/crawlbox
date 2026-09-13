package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"example.org/crawler/manager/internal/fileutil"
	"example.org/crawler/manager/internal/web"
	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"
)

const setupPage = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Crawlbox 初始设置</title><style>body{font:16px system-ui;max-width:440px;margin:10vh auto;padding:24px}label{display:block;margin:18px 0}input,button{box-sizing:border-box;width:100%;padding:12px;font:inherit}button{cursor:pointer}p{line-height:1.6}</style><h1>欢迎使用 Crawlbox</h1><p>创建管理员账号，开始管理采集和历史归档。</p><form method="post" action="/ui/setup"><label>用户名<input name="username" required maxlength="128" autocomplete="username" value="admin"></label><label>密码<input name="password" type="password" required minlength="12" maxlength="72" autocomplete="new-password"></label><label>确认密码<input name="confirm" type="password" required autocomplete="new-password"></label><p>密码需为 12–72 字节。</p><button>创建管理员</button></form></html>`

func ensureAdministrator(ctx context.Context, data string) error {
	credentials := filepath.Join(data, "admin.yaml")
	if _, err := os.Stat(credentials); err == nil {
		_, err = web.LoadCredentials(credentials)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	done := make(chan struct{})
	srv := &http.Server{Addr: setupAddress(), Handler: setupHandler(data, func() { close(done) }), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	result := make(chan error, 1)
	go func() { result <- srv.ListenAndServe() }()
	slog.Info("open /ui/ to create the first administrator", "address", srv.Addr)
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = srv.Close()
		return ctx.Err()
	case <-done:
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return err
		}
		slog.Info("administrator created; starting collection manager")
		return nil
	}
}
func setupHandler(data string, completed func()) http.Handler {
	var mu sync.Mutex
	created := false
	page := template.Must(template.New("setup").Parse(setupPage))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusSeeOther) })
	mux.HandleFunc("GET /ui/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, nil)
	})
	mux.HandleFunc("POST /ui/setup", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if created {
			http.Error(w, "管理员已经创建，请刷新页面登录。", http.StatusConflict)
			return
		}
		credentials := filepath.Join(data, "admin.yaml")
		if _, err := os.Stat(credentials); err == nil {
			http.Error(w, "管理员已经存在。", http.StatusConflict)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "无法读取账号配置。", 500)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "表单无效。", 400)
			return
		}
		username := strings.TrimSpace(r.PostForm.Get("username"))
		password := r.PostForm.Get("password")
		if username == "" || len(username) > 128 || strings.ContainsAny(username, ":\r\n") || len(password) < 12 || len(password) > 72 || password != r.PostForm.Get("confirm") {
			http.Error(w, "请检查用户名、密码长度及两次密码是否一致。", 400)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "无法创建账号。", 500)
			return
		}
		b, err := yaml.Marshal(web.Credentials{Username: username, PasswordHash: string(hash)})
		if err != nil {
			http.Error(w, "无法创建账号。", 500)
			return
		}
		if err = fileutil.AtomicWrite(credentials, b); err != nil {
			http.Error(w, "无法保存账号。", 500)
			return
		}
		created = true
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="5;url=/ui/"><title>Crawlbox</title><p>管理员已创建。正在启动服务，稍后将返回登录页面。</p><a href="/ui/">进入 Crawlbox</a>`))
		completed()
	})
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		protected.ServeHTTP(w, r)
	})
}

// resetPassword is an operator-only CLI operation; it never opens web setup.
func resetPassword(path string, input io.Reader) error {
	credentials, err := web.LoadCredentials(path)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(input, 74))
	if err != nil {
		return err
	}
	password := strings.TrimSuffix(string(b), "\n")
	if len(password) < 12 || len(password) > 72 {
		return fmt.Errorf("password must contain 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	credentials.PasswordHash = string(hash)
	b, err = yaml.Marshal(credentials)
	if err != nil {
		return err
	}
	return fileutil.AtomicWrite(path, b)
}

func setupAddress() string {
	if address := os.Getenv("CRAWLBOX_SETUP_LISTEN"); address != "" {
		return address
	}
	return ":8080"
}
